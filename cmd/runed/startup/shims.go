package startup

import (
	"context"

	"github.com/runestack/rune/pkg/log"
	acmesvc "github.com/runestack/rune/pkg/networking/acme"
	"github.com/runestack/rune/pkg/networking/ingress"
	"github.com/runestack/rune/pkg/types"
)

// Adapters that let a phase hand a collaborator something before its real
// implementation exists, or bridge two interfaces that do not quite line up.
// They live here rather than in a phase file because they belong to no single
// phase — and because the ordering guard matches phase files by source text, so
// a shim's doc comment must not sit in one.

type acmeCertStoreWithReload struct {
	store  acmesvc.CertStore
	loader *ingress.CertLoader
}

func (w acmeCertStoreWithReload) Set(ctx context.Context, host string, cert, key []byte) error {
	if err := w.store.Set(ctx, host, cert, key); err != nil {
		return err
	}
	return w.loader.Reload(ctx, host)
}

func (w acmeCertStoreWithReload) Get(ctx context.Context, host string) ([]byte, []byte, error) {
	return w.store.Get(ctx, host)
}

func (w acmeCertStoreWithReload) Delete(ctx context.Context, host string) error {
	w.loader.Forget(host)
	return w.store.Delete(ctx, host)
}

// notReadyMountResolver is the pre-agent MountResolver the orchestrator
// is seeded with at startup. Every lookup returns ("", false) so the
// instance controller treats every volume as "not yet mounted" and
// retries on the next reconcile tick — the documented transient
// condition. When the agent volumes subsystem comes up it calls
// SetMountResolver again with its real implementation, replacing this
// stub. Without it, the few seconds between apiServer.Start and the
// agent's Subsystem registration race: the controller sees a nil
// resolver, falls through to using Volume.Handle as the bind source,
// and cloud-driver volumes (where Handle is a UUID, not a path) fail
// with "invalid mount path". RUNE-BUG-DOVOLUME-ATTACH-NOOP-AND-MOUNT-PERMS.
type notReadyMountResolver struct{}

func (notReadyMountResolver) MountTargetFor(string) (string, bool) {
	return "", false
}

// acmeNoopStatus discards status updates. Until the service-watch
// wiring lands, there is no Service object to mutate; the orchestrator
// still records state in its in-memory tracker which is enough for
// observability via /metrics.
type acmeNoopStatus struct {
	logger log.Logger
}

func (s acmeNoopStatus) UpdateIngressCert(_ context.Context, ns, name string, st types.IngressCertStatus) error {
	s.logger.Debug("ingress cert status",
		log.Str("namespace", ns),
		log.Str("service", name),
		log.Str("host", st.Host),
		log.Str("state", string(st.State)),
		log.Str("error", st.LastError))
	return nil
}
