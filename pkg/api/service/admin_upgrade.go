package service

import (
	"context"

	"github.com/runestack/rune/pkg/api/authctx"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UpgradeStager stages an in-band server upgrade (RUNE-321): download,
// digest-check and manifest under the data dir, then the `ready` trigger
// the root applier consumes. Implemented by pkg/upgrade.Stager; kept as a
// local interface so AdminService stays independent of the upgrade package.
type UpgradeStager interface {
	Stage(ctx context.Context, version, sha256, requester string, allowDowngrade bool) error
}

// SetUpgradeStager plugs in the live stager. A server without one (dev
// builds that predate wiring) answers Unimplemented, which the CLI treats
// like a pre-RUNE-321 server.
func (s *AdminService) SetUpgradeStager(u UpgradeStager) { s.upgrader = u }

// UpgradeServer stages an upgrade of this server's own binaries. The apply
// — swap, restart, verify, rollback — happens out-of-process in the root
// applier; the connection is expected to drop shortly after this replies.
func (s *AdminService) UpgradeServer(ctx context.Context, req *generated.UpgradeServerRequest) (*generated.UpgradeServerResponse, error) {
	if s.upgrader == nil {
		return nil, status.Error(codes.Unimplemented, "this server does not support in-band upgrade")
	}
	if req.GetVersion() == "" {
		return nil, status.Error(codes.InvalidArgument, "version is required")
	}
	requester := authctx.SubjectFrom(ctx)
	s.logger.Info("Staging server upgrade",
		log.Str("version", req.GetVersion()),
		log.Str("requester", requester),
		log.Bool("allowDowngrade", req.GetAllowDowngrade()))

	err := s.upgrader.Stage(ctx, req.GetVersion(), req.GetSha256(), requester, req.GetAllowDowngrade())
	if err != nil {
		// The staging error's "reason=<slug>" prefix is the
		// machine-readable precondition (no-systemd | units-missing |
		// upgrade-in-progress); the CLI picks its degrade path from it.
		if _, ok := err.(interface{ PreconditionReason() string }); ok { //nolint:errorlint // Stage returns the error unwrapped
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "staging upgrade: %v", err)
	}
	return &generated.UpgradeServerResponse{StagedVersion: req.GetVersion()}, nil
}
