package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

// portForwardOptions are CLI flags for `rune port-forward` (RUNE-122).
type portForwardOptions struct {
	cmdOptions

	bindAddr string
	instance string
	detach   bool // -d: hand off to the daemon (RUNE-123)
}

// portMapping is one parsed [LOCAL:]REMOTE pair.
type portMapping struct {
	local  uint16
	remote uint16
}

func newPortForwardCmd() *cobra.Command {
	opts := &portForwardOptions{}
	cmd := &cobra.Command{
		Use:   "port-forward TARGET [LOCAL:]REMOTE [more ports ...]",
		Short: "Forward one or more local ports to a service or instance",
		Long: `Forward local TCP ports to ports on a running service or instance.

TARGET can be a service name or an instance ID; the namespace is taken from
the --namespace flag (default: current context).

PORTSPEC can be either a single port "N" (same on both sides) or "LOCAL:REMOTE"
to map a different local port. Multiple specs may be given.

Examples:
  # Forward local 27017 to mongo:27017 in the default namespace
  rune port-forward mongo 27017

  # Map a different local port (avoids collision with a local mongod)
  rune port-forward mongo 27018:27017 -n shared

  # Multiple ports
  rune port-forward flo 9002 9001 -n shared

  # Expose the forward to the LAN (default is 127.0.0.1)
  rune port-forward --bind 0.0.0.0 mongo 27017

  # Pin to a specific instance for a scaled service
  rune port-forward --instance mongo-1 mongo 27017
`,
		Aliases: []string{"pf"},
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.namespace = effectiveCmdNS(opts.namespace)
			return runPortForward(cmd.Context(), args, opts)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "", "namespace of the service/instance")
	cmd.Flags().StringVar(&opts.bindAddr, "bind", "127.0.0.1", "local bind address")
	cmd.Flags().StringVar(&opts.instance, "instance", "", "pin to a specific instance ID (for scale>1)")
	cmd.Flags().BoolVarP(&opts.detach, "detach", "d", false, "background the forward to the rune port-forward daemon")

	cmd.AddCommand(newPortForwardListCmd())
	cmd.AddCommand(newPortForwardStopCmd())
	cmd.AddCommand(newPortForwardLogsCmd())

	return cmd
}

func init() { rootCmd.AddCommand(newPortForwardCmd()) }

func runPortForward(ctx context.Context, args []string, opts *portForwardOptions) error {
	target := args[0]
	mappings, err := parsePortMappings(args[1:])
	if err != nil {
		return err
	}
	ports := make([]uint32, 0, len(mappings))
	for _, m := range mappings {
		ports = append(ports, uint32(m.remote))
	}

	if opts.detach {
		return runPortForwardDetached(target, mappings, opts)
	}

	apiClient, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to create API client: %w", err)
	}
	defer apiClient.Close()

	// Reuse the shared resolver so `service/X`, `instance/Y`, and
	// bare names behave consistently with `rune exec` and `rune logs`.
	resolved, err := resolveResourceTarget(apiClient, target, opts.namespace)
	if err != nil {
		return fmt.Errorf("failed to resolve target: %w", err)
	}

	pfClient := client.NewPortForwardClient(apiClient)

	// SIGINT / SIGTERM → cancel context → close session.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "port-forward: stopping")
			cancel()
		case <-ctx.Done():
		}
	}()

	tgt := client.PortForwardTarget{
		Namespace: resolved.namespace,
		Pin:       opts.instance,
		Ports:     ports,
	}
	if resolved.targetType == types.ResourceTypeInstance {
		tgt.InstanceID = resolved.target
	} else {
		tgt.Service = resolved.target
	}

	sess, ready, err := pfClient.Open(ctx, tgt)
	if err != nil {
		return err
	}
	defer sess.Close()

	displayTarget := ready.ServiceName
	if displayTarget == "" {
		displayTarget = ready.InstanceID
	}
	fmt.Fprintf(os.Stderr, "Forwarding to %s/%s (instance %s)\n", ready.Namespace, displayTarget, ready.InstanceID)

	return runForwardLoop(ctx, sess, opts.bindAddr, mappings)
}

// runForwardLoop binds local listeners and drives the bidi stream.
func runForwardLoop(
	ctx context.Context,
	sess *client.PortForwardSession,
	bindAddr string,
	mappings []portMapping,
) error {
	router := newConnRouter()

	// Bind every local listener first so binding errors surface
	// before the operator sees any "Forwarding ..." output that
	// might be misleading.
	listeners := make([]net.Listener, 0, len(mappings))
	for _, m := range mappings {
		addr := net.JoinHostPort(bindAddr, strconv.Itoa(int(m.local)))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return fmt.Errorf("bind %s: %w", addr, err)
		}
		fmt.Fprintf(os.Stderr, "  %s -> :%d\n", ln.Addr(), m.remote)
		listeners = append(listeners, ln)
	}

	// Receive loop: demultiplex frames from server into per-conn
	// channels owned by the router.
	recvDone := make(chan error, 1)
	go func() {
		for {
			msg, err := sess.Recv()
			if err != nil {
				recvDone <- err
				return
			}
			router.dispatch(msg)
		}
	}()

	// Accept loop per listener.
	var acceptWG sync.WaitGroup
	for i, ln := range listeners {
		acceptWG.Add(1)
		go func(ln net.Listener, remote uint16) {
			defer acceptWG.Done()
			for {
				c, err := ln.Accept()
				if err != nil {
					if !errors.Is(err, net.ErrClosed) {
						fmt.Fprintf(os.Stderr, "accept error: %v\n", err)
					}
					return
				}
				go handleLocalConn(ctx, sess, router, c, remote)
			}
		}(ln, mappings[i].remote)
	}

	// Block on context or server stream end.
	var sessionErr error
	select {
	case <-ctx.Done():
	case err := <-recvDone:
		if err != nil && err != io.EOF {
			sessionErr = err
		}
	}

	// Tear down local listeners; this unblocks accept loops.
	for _, l := range listeners {
		_ = l.Close()
	}
	acceptWG.Wait()
	router.closeAll()
	return sessionErr
}

// handleLocalConn services one accepted local connection.
func handleLocalConn(
	ctx context.Context,
	sess *client.PortForwardSession,
	router *connRouter,
	local net.Conn,
	remotePort uint16,
) {
	defer local.Close()

	connID := router.next()
	dataCh, closeCh := router.register(connID)
	defer router.unregister(connID)

	if err := sess.SendOpen(connID, uint32(remotePort)); err != nil {
		return
	}

	// Close the local socket when the remote side closes the conn or
	// the session ends, so the local→remote loop below unblocks from
	// local.Read() instead of hanging until the caller times out.
	go func() {
		select {
		case <-closeCh:
		case <-ctx.Done():
		}
		_ = local.Close()
	}()

	// remote → local pump
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case payload, ok := <-dataCh:
				if !ok {
					return
				}
				if _, err := local.Write(payload); err != nil {
					_ = sess.SendClose(connID, err.Error())
					return
				}
			}
		}
	}()

	// local → remote pump (blocking on local Read).
	buf := make([]byte, 16*1024)
	for {
		n, err := local.Read(buf)
		if n > 0 {
			payload := make([]byte, n)
			copy(payload, buf[:n])
			if sendErr := sess.SendData(connID, payload); sendErr != nil {
				return
			}
		}
		if err != nil {
			_ = sess.SendClose(connID, "")
			return
		}
	}
}

// connRouter demultiplexes server frames per conn_id.
type connRouter struct {
	mu     sync.Mutex
	nextID uint64
	conns  map[uint64]*routedConn
}

type routedConn struct {
	data  chan []byte
	close chan struct{}
	once  sync.Once
}

func newConnRouter() *connRouter {
	return &connRouter{conns: map[uint64]*routedConn{}}
}

func (r *connRouter) next() uint64 {
	return atomic.AddUint64(&r.nextID, 1)
}

func (r *connRouter) register(id uint64) (<-chan []byte, <-chan struct{}) {
	rc := &routedConn{
		data:  make(chan []byte, 32),
		close: make(chan struct{}),
	}
	r.mu.Lock()
	r.conns[id] = rc
	r.mu.Unlock()
	return rc.data, rc.close
}

func (r *connRouter) unregister(id uint64) {
	r.mu.Lock()
	rc, ok := r.conns[id]
	delete(r.conns, id)
	r.mu.Unlock()
	if ok {
		rc.once.Do(func() {
			close(rc.close)
			close(rc.data)
		})
	}
}

func (r *connRouter) dispatch(msg *generated.PortForwardServerMessage) {
	switch m := msg.GetMessage().(type) {
	case *generated.PortForwardServerMessage_Data:
		r.mu.Lock()
		rc, ok := r.conns[m.Data.ConnId]
		r.mu.Unlock()
		if !ok {
			return
		}
		select {
		case rc.data <- m.Data.Payload:
		case <-rc.close:
		}
	case *generated.PortForwardServerMessage_Close:
		r.mu.Lock()
		rc, ok := r.conns[m.Close.ConnId]
		r.mu.Unlock()
		if !ok {
			return
		}
		rc.once.Do(func() {
			close(rc.close)
			close(rc.data)
		})
	case *generated.PortForwardServerMessage_Status:
		fmt.Fprintf(os.Stderr, "port-forward status: %s\n", m.Status.GetMessage())
	}
}

func (r *connRouter) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rc := range r.conns {
		rc.once.Do(func() {
			close(rc.close)
			close(rc.data)
		})
	}
	r.conns = map[uint64]*routedConn{}
}

// parsePortMappings parses kubectl-style "N" or "L:R" specs.
func parsePortMappings(specs []string) ([]portMapping, error) {
	out := make([]portMapping, 0, len(specs))
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("empty port spec")
		}
		var localStr, remoteStr string
		if i := strings.IndexByte(s, ':'); i >= 0 {
			localStr = s[:i]
			remoteStr = s[i+1:]
		} else {
			localStr = s
			remoteStr = s
		}
		l, err := parsePort(localStr)
		if err != nil {
			return nil, fmt.Errorf("invalid local port %q: %w", localStr, err)
		}
		r, err := parsePort(remoteStr)
		if err != nil {
			return nil, fmt.Errorf("invalid remote port %q: %w", remoteStr, err)
		}
		out = append(out, portMapping{local: l, remote: r})
	}
	return out, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("port must be > 0")
	}
	return uint16(n), nil
}
