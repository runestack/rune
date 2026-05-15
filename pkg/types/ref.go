package types

import (
	"fmt"
	"net/url"
	"strings"
)

// ResourceRef is a canonical reference for resources with strongly-typed fields.
// Name form:    rune://<type>/<namespace>/<name>/<key>
// ID form:      rune://<type>/<namespace>/id/<id>/<key>
// FQDN form:    <type>:<name>.<namespace>.rune[/key]
// Minimal form: <type>:<name>[/key]
// Bare form:    <name> — Type and Namespace come from caller context;
//
//	accepted via ParseResourceRefWithDefaults only.
type ResourceRef struct {
	Type      ResourceType
	Namespace string
	Name      string
	ID        string
	Key       string
}

func (r ResourceRef) IsByID() bool    { return r.ID != "" }
func (r ResourceRef) HasKey() bool    { return r.Key != "" }
func (r ResourceRef) IsNameRef() bool { return r.Name != "" }
func (r ResourceRef) WithDefaultNamespace(ns string) ResourceRef {
	if r.Namespace == "" {
		r.Namespace = ns
	}
	return r
}

func (r ResourceRef) ToURI() string {
	if r.IsByID() {
		if r.HasKey() {
			return fmt.Sprintf("rune://%s/%s/id/%s/%s", string(r.Type), r.Namespace, url.PathEscape(r.ID), url.PathEscape(r.Key))
		}
		return fmt.Sprintf("rune://%s/%s/id/%s", string(r.Type), r.Namespace, url.PathEscape(r.ID))
	}
	if r.HasKey() {
		return fmt.Sprintf("rune://%s/%s/%s/%s", string(r.Type), r.Namespace, url.PathEscape(r.Name), url.PathEscape(r.Key))
	}
	return fmt.Sprintf("rune://%s/%s/%s", string(r.Type), r.Namespace, url.PathEscape(r.Name))
}

func (r ResourceRef) ToFetchRef() string {
	if r.IsByID() {
		return fmt.Sprintf("rune://%s/%s/id/%s", string(r.Type), r.Namespace, url.PathEscape(r.ID))
	}
	return fmt.Sprintf("rune://%s/%s/%s", string(r.Type), r.Namespace, url.PathEscape(r.Name))
}

func FormatRef(rt ResourceType, ns, name string) string {
	return fmt.Sprintf("rune://%s/%s/%s", string(rt), ns, url.PathEscape(name))
}

func FormatIDRef(rt ResourceType, ns, id string) string {
	return fmt.Sprintf("rune://%s/%s/id/%s", string(rt), ns, url.PathEscape(id))
}

// parseResourceRefInternal parses a complete canonical resource reference.
// Supports the following formats:
//  1. URI format: "rune://<type>/<namespace>/<name>/<key>" or "rune://<type>/<namespace>/id/<id>/<key>"
//     - <key> is optional in both forms
//  2. FQDN shorthand: "<type>:<name>.<namespace>.rune/<key>"
//     - <key> is optional
//  3. Minimal shorthand: "<type>:<name>/<key>" or "<type>:<name>"
//     - Namespace will be empty; caller should supply a default if needed
//
// The bare-name form ("<name>" with no scheme and no ':') is not handled
// here because it requires the caller to supply a default Type — see
// ParseResourceRefWithDefaults for that entry point.
//
// Returns a ResourceRef with Type, Namespace, Name/ID, and optional Key fields.
func parseResourceRefInternal(s string) (ResourceRef, error) {
	if strings.HasPrefix(s, "rune://") {
		u, err := url.Parse(s)
		if err != nil {
			return ResourceRef{}, err
		}
		rt := ResourceType(strings.TrimPrefix(u.Host, ""))
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) < 2 {
			return ResourceRef{}, fmt.Errorf("invalid ref: %s", s)
		}
		ns := parts[0]
		if len(parts) >= 3 && parts[1] == "id" {
			id, _ := url.PathUnescape(parts[2])
			if id == "" {
				return ResourceRef{}, fmt.Errorf("invalid ref: missing id in %s", s)
			}
			var key string
			if len(parts) >= 4 {
				key, _ = url.PathUnescape(parts[3])
			}
			return ResourceRef{Type: rt, Namespace: ns, ID: id, Key: key}, nil
		}
		name, _ := url.PathUnescape(parts[1])
		var key string
		if len(parts) >= 3 {
			key, _ = url.PathUnescape(parts[2])
		}
		return ResourceRef{Type: rt, Namespace: ns, Name: name, Key: key}, nil
	}
	// FQDN shorthand: type:name.namespace.rune[/key]
	// Allow an optional "/key" suffix similar to env var refs
	var suffixKey string
	if slash := strings.IndexByte(s, '/'); slash >= 0 {
		suffixKey = s[slash+1:]
		s = s[:slash]
	}
	if i := strings.IndexByte(s, ':'); i > 0 {
		typ := s[:i]
		rest := s[i+1:]
		// FQDN if it ends with .rune, otherwise minimal type:name form
		if strings.HasSuffix(rest, ".rune") {
			segs := strings.Split(rest, ".")
			if len(segs) < 3 || segs[len(segs)-1] != "rune" {
				return ResourceRef{}, fmt.Errorf("invalid fqdn: %s", s)
			}
			ns := segs[len(segs)-2]
			name := strings.Join(segs[:len(segs)-2], ".")
			if ns == "" || name == "" {
				return ResourceRef{}, fmt.Errorf("invalid fqdn: missing name or namespace in %s", s)
			}
			return ResourceRef{Type: ResourceType(typ), Namespace: ns, Name: name, Key: suffixKey}, nil
		}
		// Minimal shorthand: <type>:<name>[/key]
		if rest == "" {
			return ResourceRef{}, fmt.Errorf("invalid minimal ref: missing name in %s", s)
		}
		return ResourceRef{Type: ResourceType(typ), Name: rest, Key: suffixKey}, nil
	}
	return ResourceRef{}, fmt.Errorf("unknown ref format: %s", s)
}

// ParseResourceRef parses a reference and returns a ResourceRef.
func ParseResourceRef(s string) (ResourceRef, error) {
	rr, err := parseResourceRefInternal(s)
	if err != nil {
		return ResourceRef{}, err
	}
	return rr, nil
}

func ParseResourceRefWithDefaultNamespace(s, defaultNamespace string) (ResourceRef, error) {
	rr, err := parseResourceRefInternal(s)
	if err != nil {
		return ResourceRef{}, err
	}
	return rr.WithDefaultNamespace(defaultNamespace), nil
}

// ParseResourceRefWithDefaults parses s as a ResourceRef and fills in
// defaultType / defaultNamespace for fields the input does not specify.
//
// In addition to the canonical forms, a bare name (no "rune://", no ":",
// no "/") is accepted and treated as Name. Callers supply the implicit
// Type via defaultType — e.g., a fromSecret field passes
// ResourceTypeSecret. An empty defaultType is allowed; the resulting
// ResourceRef simply has an empty Type for callers that don't care.
func ParseResourceRefWithDefaults(s string, defaultType ResourceType, defaultNamespace string) (ResourceRef, error) {
	if s == "" {
		return ResourceRef{}, fmt.Errorf("empty ref")
	}
	if !strings.HasPrefix(s, "rune://") && strings.IndexByte(s, ':') < 0 {
		if strings.IndexByte(s, '/') >= 0 {
			return ResourceRef{}, fmt.Errorf("invalid ref %q: bare name must not contain '/'", s)
		}
		return ResourceRef{Type: defaultType, Namespace: defaultNamespace, Name: s}, nil
	}
	rr, err := parseResourceRefInternal(s)
	if err != nil {
		return ResourceRef{}, err
	}
	if rr.Type == "" {
		rr.Type = defaultType
	}
	return rr.WithDefaultNamespace(defaultNamespace), nil
}
