// Package driverparams resolves secret references inside the
// parameter maps the controller / agent build for OpContext, so
// drivers receive resolved plaintext values rather than refs.
//
// Why a separate package: both the volume controller, the snapshot
// controller and the node-side agent need this helper, and it sits
// at the storage-driver boundary rather than inside any one of them.
// pkg/storage/driverparams keeps the resolver out of the orchestrator
// and out of internal/agent so neither depends on the other.
//
// Reference syntax: any parameter value that parses as a
// types.ResourceRef of type "secret" (e.g. `secret:do-api-token/token`
// or the fully-qualified `secret:do-api-token.shared.rune/token`) is
// resolved against the supplied SecretLookup; the resolved value
// replaces the reference in the returned map. Values that don't
// parse as a secret ref are passed through verbatim — drivers see
// the original string. See RUNE-200 PR 3.
package driverparams

import (
	"context"
	"fmt"

	"github.com/runestack/rune/pkg/types"
)

// SecretLookup resolves one field of a Rune Secret to plaintext.
// Mirrors the historical dovolume.SecretLookup signature so the same
// closure can be re-used at all three call sites (volume controller,
// snapshot controller, node agent) without bespoke wrapper types.
type SecretLookup func(ctx context.Context, namespace, name, key string) (string, error)

// Resolve walks params, replacing any secret-ref-shaped value with
// the resolved secret value. Returns a NEW map; the input is not
// mutated. Values that aren't secret refs are copied through.
//
// defaultNamespace is applied to refs that omit one (e.g. the
// minimal shorthand `secret:do-api-token/token`). Passing "" means
// "no default" — refs without an explicit namespace fail to
// resolve.
//
// lookup may be nil; in that case secret-ref values fail with a
// clear error rather than being passed through silently. This is
// the right default at every call site — silently passing through a
// `secret:...` literal to a driver would result in a confusing
// downstream failure ("invalid token: malformed bearer string").
//
// Non-secret refs (configmap, service, etc.) and parse failures
// pass through verbatim — they're not secrets and the driver may
// have its own use for them.
func Resolve(ctx context.Context, params map[string]string, defaultNamespace string, lookup SecretLookup) (map[string]string, error) {
	if len(params) == 0 {
		return params, nil
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		resolved, err := resolveOne(ctx, k, v, defaultNamespace, lookup)
		if err != nil {
			return nil, err
		}
		out[k] = resolved
	}
	return out, nil
}

func resolveOne(ctx context.Context, key, value, defaultNamespace string, lookup SecretLookup) (string, error) {
	ref, err := types.ParseResourceRef(value)
	if err != nil {
		// Not a ref at all — plain literal value. Pass through.
		return value, nil
	}
	if ref.Type != types.ResourceTypeSecret {
		// Some other resource ref (configmap, service, etc.). Not our
		// job here; pass through and let the driver decide whether to
		// reject it.
		return value, nil
	}
	if lookup == nil {
		return "", fmt.Errorf("driverparams: parameter %q references a secret (%s) but no SecretLookup is wired", key, value)
	}
	ns := ref.Namespace
	if ns == "" {
		ns = defaultNamespace
	}
	if ns == "" {
		return "", fmt.Errorf("driverparams: parameter %q secret ref %q omits namespace and no default is set", key, value)
	}
	if ref.Name == "" {
		return "", fmt.Errorf("driverparams: parameter %q secret ref %q has empty name", key, value)
	}
	if ref.Key == "" {
		return "", fmt.Errorf("driverparams: parameter %q secret ref %q omits the '/<key>' suffix", key, value)
	}
	resolved, err := lookup(ctx, ns, ref.Name, ref.Key)
	if err != nil {
		return "", fmt.Errorf("driverparams: resolve secret ref %q for parameter %q: %w", value, key, err)
	}
	return resolved, nil
}
