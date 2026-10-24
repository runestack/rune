// Package authz holds the authorization checks that must hold for a
// resource payload regardless of which entry point produced it.
//
// It exists because the previous home for these checks — a switch on
// gRPC method names in the server's RBAC interceptor — can only see
// inbound RPCs. `rune cast` reaches ServiceService's effects through
// ReleaseService/Cast, whose applier calls the orchestrator in-process,
// where no interceptor runs. Admission-shaped rules therefore live here,
// on the typed payload, and are enforced at the choke point every entry
// point shares, so coverage does not depend on which transport was used.
package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/runestack/rune/pkg/api/authctx"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/types"
)

// Requirement is a (resource, verb) pair the subject must be authorized
// for, on top of whatever verb the entry point itself demanded.
type Requirement struct {
	Resource string
	Verb     string
}

// Authorizer answers whether subjectID may perform verb on resource in
// namespace. Implemented by the store-backed policy evaluator; the API
// server's interceptor uses the same implementation.
type Authorizer interface {
	Authorize(ctx context.Context, subjectID, resource, verb, namespace string) (bool, error)
}

// DeniedError reports a failed admission requirement. Callers at RPC
// boundaries map it to codes.PermissionDenied.
type DeniedError struct {
	SubjectID string
	Requirement
	Namespace string
}

func (e *DeniedError) Error() string {
	subject := e.SubjectID
	if subject == "" {
		subject = "unauthenticated caller"
	}
	ns := e.Namespace
	if ns == "" {
		ns = "*"
	}
	return fmt.Sprintf("permission denied: %s is not allowed to %q %s in namespace %q",
		subject, e.Verb, e.Resource, ns)
}

// IsDenied reports whether err is (or wraps) a DeniedError.
func IsDenied(err error) bool {
	var d *DeniedError
	return errors.As(err, &d)
}

// ServiceRequirements returns the extra requirements a service payload
// must satisfy. The security knobs are read through
// types.SecurityContext.RequiresPrivilegedGate so main container and
// init steps can never drift apart, and so there is exactly one
// definition of "this needs the privileged verb" in the tree.
func ServiceRequirements(svc *types.Service) []Requirement {
	if svc == nil {
		return nil
	}
	if svc.SecurityContext.RequiresPrivilegedGate() {
		return []Requirement{{Resource: "services", Verb: "privileged"}}
	}
	for i := range svc.InitSteps {
		if svc.InitSteps[i].SecurityContext.RequiresPrivilegedGate() {
			return []Requirement{{Resource: "services", Verb: "privileged"}}
		}
	}
	return nil
}

// releaseResources names the RBAC resource for each kind a release writes.
var releaseResources = map[types.ResourceType]string{
	types.ResourceTypeService:   "services",
	types.ResourceTypeSecret:    "secrets",
	types.ResourceTypeConfigmap: "configmaps",
	types.ResourceTypeVolume:    "volumes",
}

// ReleaseChangeRequirement returns the verb a planned release change needs
// on its resource: the one the direct RPC for the same write demands, so a
// release can never do what its caller could not do directly. `releases:
// create` alone would otherwise let a cast-scoped token adopt and then prune
// any service in reach. Adopt is an update of an existing resource; prune is
// a delete. ok is false only for references to shared cluster kinds, which
// write nothing. A kind missing from the table is named by its raw type, a
// resource no narrow policy grants, so a new kind fails closed.
func ReleaseChangeRequirement(c release.PlannedChange) (Requirement, bool) {
	var verb string
	switch c.Action {
	case release.ActionCreate:
		verb = "create"
	case release.ActionUpdate, release.ActionAdopt:
		verb = "update"
	case release.ActionPrune:
		verb = "delete"
	default:
		return Requirement{}, false
	}
	res, ok := releaseResources[c.Ref.ResourceType]
	if !ok {
		res = string(c.Ref.ResourceType)
	}
	return Requirement{Resource: res, Verb: verb}, true
}

// Gate evaluates admission requirements against the subject carried on
// the context. A nil Gate admits everything, which is how servers with
// auth disabled (and the many in-process tests) keep working: the gate
// is installed only where authentication actually stamps a subject.
type Gate struct{ auth Authorizer }

// NewGate returns a Gate backed by auth. A nil Authorizer yields a nil
// Gate so the caller cannot accidentally install a gate that has no way
// to answer and would deny every write.
func NewGate(auth Authorizer) *Gate {
	if auth == nil {
		return nil
	}
	return &Gate{auth: auth}
}

// AdmitService checks svc against the service admission requirements: the
// privileged verb in its own namespace, and read access in every other
// namespace it references. It fails closed: with a gate installed, a payload
// that needs either and a context with no subject is denied.
func (g *Gate) AdmitService(ctx context.Context, svc *types.Service) error {
	if g == nil || IsSystem(ctx) {
		return nil
	}
	ns := ""
	if svc != nil {
		ns = svc.Namespace
	}
	for _, r := range ServiceRequirements(svc) {
		if err := g.require(ctx, r, ns); err != nil {
			return err
		}
	}
	for _, r := range ServiceReferenceRequirements(svc) {
		if err := g.require(ctx, r.Requirement, r.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// AdmitReleaseChanges checks every change against the subject, each in its
// own resource's namespace, and stops at the first denial. Callers run it
// over the whole plan before applying anything, so a denial never lands
// mid-apply. A ref's namespace, not the release's, is the one checked: a
// castfile may place resources outside the release namespace.
func (g *Gate) AdmitReleaseChanges(ctx context.Context, changes []release.PlannedChange) error {
	if g == nil || IsSystem(ctx) {
		return nil
	}
	for _, c := range changes {
		r, ok := ReleaseChangeRequirement(c)
		if !ok {
			continue
		}
		if err := g.require(ctx, r, c.Ref.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// AdmitReleaseReads checks that the subject may read every resource a plan
// names, each in its own namespace. The dry-run Plan path uses it, so a
// preview reveals nothing about a namespace the caller cannot read.
func (g *Gate) AdmitReleaseReads(ctx context.Context, changes []release.PlannedChange) error {
	if g == nil || IsSystem(ctx) {
		return nil
	}
	for _, c := range changes {
		r, ok := ReleaseChangeRequirement(c)
		if !ok {
			continue
		}
		r.Verb = "get"
		if err := g.require(ctx, r, c.Ref.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// require fails closed: with a gate installed, a context with no subject is
// denied.
func (g *Gate) require(ctx context.Context, r Requirement, ns string) error {
	subject := authctx.SubjectFrom(ctx)
	if subject == "" {
		return &DeniedError{Requirement: r, Namespace: ns}
	}
	ok, err := g.auth.Authorize(ctx, subject, r.Resource, r.Verb, ns)
	if err != nil {
		return fmt.Errorf("authorization error: %w", err)
	}
	if !ok {
		return &DeniedError{SubjectID: subject, Requirement: r, Namespace: ns}
	}
	return nil
}

type systemKey struct{}

// WithSystem marks ctx as an internal, non-user-initiated write that
// skips admission. The only supported use is restoring a pre-image the
// cluster already accepted — a release rollback must not be blocked by
// a verb the rolling-back subject lacks, or a failed cast would strand
// the cluster in a half-applied state. The marker is a private context
// value with no wire representation, so it cannot be set by a caller.
func WithSystem(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemKey{}, true)
}

// IsSystem reports whether ctx was marked by WithSystem.
func IsSystem(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(systemKey{}).(bool)
	return v
}
