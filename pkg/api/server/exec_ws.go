package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/repos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// Exec WebSocket bridge (RUNE-200C §3). Exec is bidirectional (stdin ↔
// stdout/stderr), which browsers cannot do over Connect/gRPC-Web. This bridges
// a browser WebSocket to the existing ExecService.StreamExec gRPC over a
// loopback self-dial, so all auth / rbac (exec:exec) / audit run server-side
// unchanged. The bridge itself is a dumb byte-pump.
//
// Wire protocol (both directions): each binary WebSocket message is a
// length-delimited-free, proto-encoded generated.ExecRequest (client→server) or
// generated.ExecResponse (server→client) — the SAME schema the gRPC API uses,
// so the SPA reuses its generated types (ExecRequest/ExecResponse.toBinary()).
//
// Auth: browsers cannot set an Authorization header on a WebSocket, so the
// bearer token rides the Sec-WebSocket-Protocol header as "rune.bearer.<token>".
// The negotiated subprotocol is always "rune.exec.v1" (never the token).
const (
	execWSPath = "/v1/exec/ws"

	execSubprotocol    = "rune.exec.v1"
	execBearerSubproto = "rune.bearer." // prefix; suffix is the token
)

// execWSHandler upgrades to a WebSocket and bridges it to ExecService.StreamExec.
func (s *APIServer) execWSHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerFromWSSubprotocols(r)

		// ui:access gate (dashboard-specific). exec:exec is enforced
		// server-side by the gRPC interceptor on the self-dial below.
		if s.options.EnableAuth {
			if token == "" {
				http.Error(w, "missing bearer token (Sec-WebSocket-Protocol rune.bearer.<token>)", http.StatusUnauthorized)
				return
			}
			tok, err := repos.NewTokenRepo(s.store).FindRequestBearer(r.Context(), token)
			if err != nil {
				http.Error(w, "invalid bearer token", http.StatusUnauthorized)
				return
			}
			allowed, err := s.evaluatePolicies(r.Context(), tok.SubjectID, "ui", "access", "")
			if err != nil {
				http.Error(w, "authorization error", http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.Error(w, "ui:access denied", http.StatusForbidden)
				return
			}
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{execSubprotocol},
		})
		if err != nil {
			s.logger.Debug("exec ws: accept failed", log.Err(err))
			return
		}
		// Exec output can burst; raise the per-message read limit from the 32KB
		// default. Individual frames stay well under this.
		conn.SetReadLimit(4 << 20)
		defer conn.CloseNow() //nolint:errcheck

		if err := s.bridgeExec(r.Context(), conn, token); err != nil {
			s.logger.Debug("exec ws: session ended", log.Err(err))
			conn.Close(websocket.StatusInternalError, "exec session error") //nolint:errcheck
			return
		}
		conn.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	}
}

// bridgeExec opens a self-dialed ExecService.StreamExec and pumps proto frames
// between it and the WebSocket until either side closes.
func (s *APIServer) bridgeExec(parent context.Context, conn *websocket.Conn, token string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	gconn, err := s.dialSelfGRPC(ctx)
	if err != nil {
		return err
	}
	defer gconn.Close() //nolint:errcheck

	execClient := generated.NewExecServiceClient(gconn)
	// Carry the bearer token so the gRPC auth + rbac (exec:exec) + audit
	// interceptors run exactly as they do for `rune exec`.
	callCtx := ctx
	if token != "" {
		callCtx = metadata.AppendToOutgoingContext(ctx, "authorization", "bearer "+token)
	}
	stream, err := execClient.StreamExec(callCtx)
	if err != nil {
		return err
	}

	errCh := make(chan error, 2)

	// WebSocket → gRPC (stdin, resize, signal, the init frame).
	go func() {
		for {
			typ, data, rerr := conn.Read(ctx)
			if rerr != nil {
				_ = stream.CloseSend()
				errCh <- rerr
				return
			}
			if typ != websocket.MessageBinary {
				continue // ignore stray text frames
			}
			var req generated.ExecRequest
			if uerr := proto.Unmarshal(data, &req); uerr != nil {
				errCh <- uerr
				return
			}
			if serr := stream.Send(&req); serr != nil {
				errCh <- serr
				return
			}
		}
	}()

	// gRPC → WebSocket (stdout, stderr, status, exit).
	go func() {
		for {
			resp, rerr := stream.Recv()
			if rerr != nil {
				errCh <- rerr // io.EOF on normal completion
				return
			}
			out, merr := proto.Marshal(resp)
			if merr != nil {
				errCh <- merr
				return
			}
			if werr := conn.Write(ctx, websocket.MessageBinary, out); werr != nil {
				errCh <- werr
				return
			}
		}
	}()

	err = <-errCh
	cancel()
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	// A normal client-initiated WS close is not an error.
	if websocket.CloseStatus(err) != -1 {
		return nil
	}
	return err
}

// dialSelfGRPC opens a client connection to this daemon's own gRPC server.
func (s *APIServer) dialSelfGRPC(_ context.Context) (*grpc.ClientConn, error) {
	target := loopbackTarget(s.options.GRPCAddr)
	var creds credentials.TransportCredentials = insecure.NewCredentials()
	if s.options.EnableTLS {
		// Loopback self-call: we are dialing ourselves, so cert-name
		// verification adds nothing — skip it. The connection never leaves
		// the host.
		creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // loopback self-dial
	}
	return grpc.NewClient(target, grpc.WithTransportCredentials(creds))
}

// loopbackTarget turns a listen address (":7863", "0.0.0.0:7863") into a
// dialable loopback target.
func loopbackTarget(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1:" + strings.TrimPrefix(addr, ":")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// bearerFromWSSubprotocols extracts the bearer token from the
// Sec-WebSocket-Protocol header value "rune.bearer.<token>".
func bearerFromWSSubprotocols(r *http.Request) string {
	for _, hv := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(hv, ",") {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, execBearerSubproto) {
				return strings.TrimPrefix(p, execBearerSubproto)
			}
		}
	}
	return ""
}
