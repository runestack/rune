package authz

import (
	"sort"
	"strings"

	"github.com/runestack/rune/pkg/types"
)

// NamespacedRequirement is a Requirement checked in a namespace other than
// the payload's own.
type NamespacedRequirement struct {
	Requirement
	Namespace string
}

// ServiceReferenceRequirements returns what a service payload needs for each
// resource it references in another namespace. The orchestrator resolves
// these references with no subject, so without this check a token scoped to
// one namespace could read another's secrets through its own container's env,
// or mount another's volumes. A secret reference needs secrets:reveal there,
// since its value lands in a container the caller controls; configmaps and
// volumes need get. References within the service's own namespace need
// nothing beyond the service verb itself.
func ServiceReferenceRequirements(svc *types.Service) []NamespacedRequirement {
	if svc == nil {
		return nil
	}
	c := refCollector{own: svc.Namespace, seen: map[NamespacedRequirement]bool{}}
	c.env(svc.Env, svc.EnvFrom)
	for i := range svc.InitSteps {
		c.env(svc.InitSteps[i].Env, svc.InitSteps[i].EnvFrom)
	}
	c.volumes(svc.Volumes)
	c.expose(svc.Expose)
	c.sort()
	return c.out
}

type refCollector struct {
	own  string
	seen map[NamespacedRequirement]bool
	out  []NamespacedRequirement
}

func (c *refCollector) env(env map[string]string, from []types.EnvFromSource) {
	for _, v := range env {
		c.envTemplates(v)
	}
	for _, src := range from {
		switch {
		case src.SecretName != "":
			c.add(types.ResourceTypeSecret, src.Namespace)
		case src.ConfigmapName != "":
			c.add(types.ResourceTypeConfigmap, src.Namespace)
		}
	}
}

func (c *refCollector) volumes(mounts []types.VolumeMount) {
	for _, m := range mounts {
		if m.Claim == nil {
			continue
		}
		if ns, _, ok := strings.Cut(m.Claim.Name, "/"); ok && ns != "" {
			c.add(types.ResourceTypeVolume, ns)
		}
	}
}

func (c *refCollector) expose(e *types.ServiceExpose) {
	if e == nil {
		return
	}
	if e.TLS != nil && e.TLS.Secret != "" {
		c.secretRef(e.TLS.Secret)
	}
	if e.ClientCert != nil && e.ClientCert.CASecret != "" {
		c.secretRef(e.ClientCert.CASecret)
	}
}

func (c *refCollector) sort() {
	sort.Slice(c.out, func(i, j int) bool {
		a, b := c.out[i], c.out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Resource < b.Resource
	})
}

func (c *refCollector) add(rt types.ResourceType, ns string) {
	if ns == "" || ns == c.own {
		return
	}
	var r Requirement
	switch rt {
	case types.ResourceTypeSecret:
		r = Requirement{Resource: "secrets", Verb: "reveal"}
	case types.ResourceTypeConfigmap:
		r = Requirement{Resource: "configmaps", Verb: "get"}
	case types.ResourceTypeVolume:
		r = Requirement{Resource: "volumes", Verb: "get"}
	default:
		return
	}
	nr := NamespacedRequirement{Requirement: r, Namespace: ns}
	if !c.seen[nr] {
		c.seen[nr] = true
		c.out = append(c.out, nr)
	}
}

// envTemplates finds {{type:ref}} placeholders the way the env resolver
// does. A placeholder that does not parse is skipped: resolution fails on it
// anyway, so it can never reach another namespace.
func (c *refCollector) envTemplates(value string) {
	for {
		open := strings.Index(value, "{{")
		if open < 0 {
			return
		}
		end := strings.Index(value[open:], "}}")
		if end < 0 {
			return
		}
		inner := strings.Join(strings.Fields(value[open+2:open+end]), "")
		if ref, err := types.ParseResourceRefWithDefaultNamespace(inner, c.own); err == nil {
			c.add(ref.Type, ref.Namespace)
		}
		value = value[open+end+2:]
	}
}

func (c *refCollector) secretRef(s string) {
	if ref, err := types.ParseResourceRefWithDefaults(s, types.ResourceTypeSecret, c.own); err == nil {
		c.add(ref.Type, ref.Namespace)
	}
}
