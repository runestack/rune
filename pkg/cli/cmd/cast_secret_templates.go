package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
)

// secretTemplateRegex matches `{{ secret:<ref> }}` placeholders in secret
// data values. The inner ref is parsed by types.ParseResourceRef and supports:
//   - secret:<name>/<key>
//   - secret:<name>.<namespace>.rune/<key>
var secretTemplateRegex = regexp.MustCompile(`\{\{\s*secret:([^{}]+?)\s*\}\}`)

// secretKey is a (namespace, name) tuple used as a map key for in-castfile
// lookups and the reveal-cache.
type secretKey struct {
	Namespace string
	Name      string
}

func (k secretKey) String() string { return k.Namespace + "/" + k.Name }

// secretRefSpec is one parsed `{{ secret:... }}` placeholder, resolved against
// the dependent secret's own namespace.
type secretRefSpec struct {
	Match     string // the full literal match, e.g. "{{ secret:db-host/value }}"
	Namespace string // resolved component namespace
	Name      string // component name
	Key       string // key inside the component's data
}

// findSecretRefs returns every `{{ secret:... }}` placeholder in s, resolved
// against defaultNamespace. Returns an error on any malformed ref or missing
// key segment (we require an explicit `/key`).
func findSecretRefs(s, defaultNamespace string) ([]secretRefSpec, error) {
	matches := secretTemplateRegex.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil, nil
	}
	refs := make([]secretRefSpec, 0, len(matches))
	for _, m := range matches {
		full := m[0]
		inner := strings.TrimSpace(m[1])
		rr, err := types.ParseResourceRef("secret:" + inner)
		if err != nil {
			return nil, fmt.Errorf("invalid secret template %q: %w", full, err)
		}
		if rr.Key == "" {
			return nil, fmt.Errorf("invalid secret template %q: missing /<key> segment", full)
		}
		ns := rr.Namespace
		if ns == "" {
			ns = defaultNamespace
		}
		refs = append(refs, secretRefSpec{
			Match:     full,
			Namespace: ns,
			Name:      rr.Name,
			Key:       rr.Key,
		})
	}
	return refs, nil
}

// secretRefsForSecret returns all template refs across every value of secret.Data.
func secretRefsForSecret(s *types.Secret) ([]secretRefSpec, error) {
	var all []secretRefSpec
	// Iterate keys in sorted order for deterministic error messages.
	keys := make([]string, 0, len(s.Data))
	for k := range s.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		refs, err := findSecretRefs(s.Data[k], s.Namespace)
		if err != nil {
			return nil, fmt.Errorf("secret %s/%s data[%q]: %w", s.Namespace, s.Name, k, err)
		}
		all = append(all, refs...)
	}
	return all, nil
}

// renderSecretTemplates resolves `{{ secret:... }}` placeholders in every secret
// in info, calling apiClient.RevealSecret for components that are not present
// in the castfile. Mutates secret.Data in place. Returns an error on cycle,
// missing component, missing key, or reveal failure.
//
// apiClient may be nil when no secrets in info reference any out-of-castfile
// components (used by tests).
func renderSecretTemplates(apiClient *client.Client, info *ResourceInfo) error {
	if info == nil {
		return nil
	}

	// Flatten all secrets from all files; build (ns, name) → *Secret index.
	var allSecrets []*types.Secret
	index := make(map[secretKey]*types.Secret)
	for _, secrets := range info.SecretsByFile {
		for _, s := range secrets {
			ns := s.Namespace
			if ns == "" {
				ns = "default"
			}
			s.Namespace = ns
			allSecrets = append(allSecrets, s)
			index[secretKey{Namespace: ns, Name: s.Name}] = s
		}
	}
	if len(allSecrets) == 0 {
		return nil
	}

	// Build dependency graph (only for in-castfile→in-castfile edges) and
	// validate every reference's syntax up front. Out-of-castfile components
	// are leaves resolved via the reveal API and don't appear in the graph.
	deps := make(map[secretKey]map[secretKey]struct{})
	hasTemplates := make(map[secretKey]bool)
	for _, s := range allSecrets {
		k := secretKey{Namespace: s.Namespace, Name: s.Name}
		refs, err := secretRefsForSecret(s)
		if err != nil {
			return err
		}
		if len(refs) == 0 {
			continue
		}
		hasTemplates[k] = true
		set := make(map[secretKey]struct{})
		for _, r := range refs {
			rk := secretKey{Namespace: r.Namespace, Name: r.Name}
			if rk == k {
				return fmt.Errorf("secret %s references itself in templated data", k)
			}
			if _, inCast := index[rk]; inCast {
				set[rk] = struct{}{}
			}
		}
		deps[k] = set
	}

	// Nothing to render?
	if len(hasTemplates) == 0 {
		return nil
	}

	// Topological sort over the in-castfile subgraph (Kahn's algorithm).
	// Cycles surface as nodes left unprocessed.
	order, err := topoSortSecrets(deps)
	if err != nil {
		return err
	}

	// Cache for reveal results across the whole render pass.
	revealCache := make(map[secretKey]map[string]string)

	// resolveComponent returns the data map for (ns, name) using either the
	// in-castfile rendered secret (if present) or the reveal API.
	resolveComponent := func(rk secretKey) (map[string]string, error) {
		if s, ok := index[rk]; ok {
			return s.Data, nil
		}
		if cached, ok := revealCache[rk]; ok {
			return cached, nil
		}
		if apiClient == nil {
			return nil, fmt.Errorf("secret %s referenced as a template component is not defined in the castfile and no API client is available to reveal it", rk)
		}
		revealed, err := client.NewSecretClient(apiClient).RevealSecret(rk.Namespace, rk.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to reveal component secret %s: %w", rk, err)
		}
		revealCache[rk] = revealed.Data
		return revealed.Data, nil
	}

	// Render in topo order. Out-of-castfile components are revealed lazily.
	// Within-castfile components are guaranteed rendered before their dependents.
	for _, k := range order {
		if !hasTemplates[k] {
			continue
		}
		s := index[k]
		if err := renderSecretValues(s, resolveComponent); err != nil {
			return err
		}
	}

	return nil
}

// renderSecretValues substitutes every `{{ secret:... }}` placeholder in
// s.Data using resolveComponent to fetch component values. Mutates s.Data.
func renderSecretValues(s *types.Secret, resolveComponent func(secretKey) (map[string]string, error)) error {
	for k, v := range s.Data {
		refs, err := findSecretRefs(v, s.Namespace)
		if err != nil {
			return err
		}
		if len(refs) == 0 {
			continue
		}
		out := v
		for _, r := range refs {
			data, err := resolveComponent(secretKey{Namespace: r.Namespace, Name: r.Name})
			if err != nil {
				return fmt.Errorf("secret %s/%s data[%q]: %w", s.Namespace, s.Name, k, err)
			}
			val, ok := data[r.Key]
			if !ok {
				return fmt.Errorf("secret %s/%s data[%q]: component secret %s/%s has no key %q", s.Namespace, s.Name, k, r.Namespace, r.Name, r.Key)
			}
			out = strings.ReplaceAll(out, r.Match, val)
		}
		s.Data[k] = out
	}
	return nil
}

// topoSortSecrets returns the secrets in dependency order (deps first). Any
// cycle is reported as an error naming the unresolved nodes.
//
// deps: node → set of nodes it depends on (edges point from dependent → dependency).
// We need dependencies emitted before dependents, so we use Kahn's algorithm
// over the *reverse* graph: start with nodes that have no incoming edges
// (i.e. nothing depends on them in the reverse graph), which are nodes that
// are themselves leaf-most dependencies in the forward graph.
func topoSortSecrets(deps map[secretKey]map[secretKey]struct{}) ([]secretKey, error) {
	// Collect all nodes that appear (as dependents).
	allNodes := make(map[secretKey]struct{})
	for k, set := range deps {
		allNodes[k] = struct{}{}
		for d := range set {
			allNodes[d] = struct{}{}
		}
	}

	// In-degree in the dependent→dependency graph: number of dependencies
	// each node has that are still unresolved.
	indeg := make(map[secretKey]int, len(allNodes))
	for n := range allNodes {
		indeg[n] = len(deps[n])
	}

	// Reverse adjacency: for each dependency, list its dependents.
	reverse := make(map[secretKey][]secretKey)
	for dependent, set := range deps {
		for dep := range set {
			reverse[dep] = append(reverse[dep], dependent)
		}
	}

	// Seed queue with nodes whose dependencies are all out-of-castfile (or none).
	// Sort for deterministic output.
	var queue []secretKey
	for n, d := range indeg {
		if d == 0 {
			queue = append(queue, n)
		}
	}
	sortSecretKeys(queue)

	var order []secretKey
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		order = append(order, n)
		next := reverse[n]
		sortSecretKeys(next)
		for _, m := range next {
			indeg[m]--
			if indeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}

	if len(order) != len(allNodes) {
		var stuck []string
		for n, d := range indeg {
			if d > 0 {
				stuck = append(stuck, n.String())
			}
		}
		sort.Strings(stuck)
		return nil, fmt.Errorf("cycle detected in cast-time secret templates among: %s", strings.Join(stuck, ", "))
	}

	return order, nil
}

func sortSecretKeys(s []secretKey) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Namespace != s[j].Namespace {
			return s[i].Namespace < s[j].Namespace
		}
		return s[i].Name < s[j].Name
	})
}
