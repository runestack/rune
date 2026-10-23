package types

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type hashClass int

const (
	// hashTemplate moves both hashes: running instances are replaced.
	hashTemplate hashClass = iota
	// hashDesired moves only the full hash: reconciled, nothing replaced.
	hashDesired
	// hashNone moves neither: identity and observed state.
	hashNone
)

func (c hashClass) String() string {
	return [...]string{"template", "desired", "none"}[c]
}

// serviceHashFields classifies every field of Service by path. The longest
// matching prefix wins, so a nested entry overrides its parent. A nested
// field nobody lists inherits its parent's class: a new field under a
// template parent replaces instances, which is the safe default, but one
// under a desired parent is assumed to be read live.
var serviceHashFields = map[string]hashClass{
	"Image":           hashTemplate,
	"Command":         hashTemplate,
	"Args":            hashTemplate,
	"Env":             hashTemplate,
	"EnvFrom":         hashTemplate,
	"Runtime":         hashTemplate,
	"Process":         hashTemplate,
	"Ports":           hashTemplate,
	"Resources":       hashTemplate,
	"Health":          hashTemplate,
	"SecretMounts":    hashTemplate,
	"ConfigmapMounts": hashTemplate,
	"Volumes":         hashTemplate,
	"InitSteps":       hashTemplate,
	"SecurityContext": hashTemplate,

	"Health.Liveness.FailureThreshold": hashDesired,
	"Volumes[].ClaimTemplate":          hashDesired,

	"Scale":              hashDesired,
	"UpdateStrategy":     hashDesired,
	"DrainSeconds":       hashDesired,
	"ImageRegistry":      hashDesired,
	"Registry":           hashDesired,
	"ImagePull":          hashDesired,
	"ImagePullAnonymous": hashDesired,
	"Labels":             hashDesired,
	"RestartPolicy":      hashDesired,
	"Expose":             hashDesired,
	"Dependencies":       hashDesired,
	"NetworkPolicy":      hashDesired,
	"Discovery":          hashDesired,
	"Affinity":           hashDesired,
	"Autoscale":          hashDesired,

	"NamespacedResource": hashNone,
	"ID":                 hashNone,
	"Name":               hashNone,
	"Namespace":          hashNone,
	"Status":             hashNone,
	"StatusReason":       hashNone,
	"StatusMessage":      hashNone,
	"IngressCert":        hashNone,
	"Instances":          hashNone,
	"Update":             hashNone,
	"Metadata":           hashNone,
	"Discovery.VIP":      hashNone,
}

func classifyHashPath(path string) (hashClass, string, bool) {
	best := ""
	for prefix := range serviceHashFields {
		if (path == prefix || strings.HasPrefix(path, prefix+".") || strings.HasPrefix(path, prefix+"[]") ||
			strings.HasPrefix(path, prefix+"{}")) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return 0, "", false
	}
	return serviceHashFields[best], best, true
}

// hashLeaf is one mutable value reachable from a Service.
type hashLeaf struct {
	path string
	// steps navigate from the Service to the value: a field index, or -1
	// for "the first slice element / the only map value".
	steps []int
}

// setScalar sets a scalar to a non-zero value, or with bump changes it from
// whatever it is. It reports false for anything that is not a scalar.
func setScalar(v reflect.Value, bump bool) bool {
	switch v.Kind() {
	case reflect.String:
		if bump {
			v.SetString(v.String() + "2")
		} else {
			v.SetString("s")
		}
	case reflect.Bool:
		v.SetBool(!bump || !v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 3)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(v.Uint() + 3)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(v.Float() + 3)
	default:
		return false
	}
	return true
}

// hashWalker fills a value with something non-zero everywhere. With classify
// set it also records every scalar it reaches as a leaf, and leaves subtrees
// classified hashNone zero, recorded as one leaf: the test fills them in and
// checks nothing moves.
type hashWalker struct {
	t        *testing.T
	classify bool
	leaves   []hashLeaf
}

func (w *hashWalker) leaf(path string, steps []int) {
	w.leaves = append(w.leaves, hashLeaf{path, append([]int(nil), steps...)})
}

func (w *hashWalker) walk(v reflect.Value, path string, steps []int) {
	w.t.Helper()
	if len(steps) > 12 {
		w.t.Fatalf("%s: nesting too deep; is the type recursive?", path)
	}
	if w.classify {
		if class, prefix, ok := classifyHashPath(path); ok && class == hashNone && prefix == path {
			w.leaf(path, steps)
			return
		}
	}
	if setScalar(v, false) {
		if w.classify {
			w.leaf(path, steps)
		}
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		w.walkStruct(v, path, steps)
	case reflect.Ptr:
		v.Set(reflect.New(v.Type().Elem()))
		w.walk(v.Elem(), path, append(steps, -1))
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		w.walk(v.Index(0), path+"[]", append(steps, -1))
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			w.t.Fatalf("%s: map key %s not supported", path, v.Type().Key())
		}
		elem := reflect.New(v.Type().Elem()).Elem()
		w.walk(elem, path+"{}", append(steps, -1))
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(reflect.ValueOf("k").Convert(v.Type().Key()), elem)
		v.Set(m)
	case reflect.Interface:
		if w.classify {
			w.t.Fatalf("%s: interface fields cannot be walked; classify it as none", path)
		}
	default:
		w.t.Fatalf("%s: kind %s not supported", path, v.Kind())
	}
}

func (w *hashWalker) walkStruct(v reflect.Value, path string, steps []int) {
	w.t.Helper()
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		p := strings.TrimPrefix(path+"."+f.Name, ".")
		before := len(w.leaves)
		w.walk(v.Field(i), p, append(steps, i))
		if w.classify && len(w.leaves) == before {
			// time.Time, map[string]struct{} and the like: nothing the
			// mutation can change, so the field would go unchecked.
			w.t.Fatalf("%s has no value the walk can change; extend setScalar", p)
		}
	}
}

// mutateHashLeaf changes the value at the end of steps, copying every
// pointer, slice and map it passes through so the base is not touched.
func mutateHashLeaf(t *testing.T, v reflect.Value, steps []int) {
	t.Helper()
	if len(steps) == 0 {
		if !setScalar(v, true) {
			// A hashNone subtree, left zero by the walk: fill it.
			(&hashWalker{t: t}).walk(v, "", nil)
		}
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		mutateHashLeaf(t, v.Field(steps[0]), steps[1:])
	case reflect.Ptr:
		cp := reflect.New(v.Type().Elem())
		cp.Elem().Set(v.Elem())
		v.Set(cp)
		mutateHashLeaf(t, cp.Elem(), steps[1:])
	case reflect.Slice:
		cp := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		reflect.Copy(cp, v)
		v.Set(cp)
		mutateHashLeaf(t, cp.Index(0), steps[1:])
	case reflect.Map:
		key := v.MapKeys()[0]
		elem := reflect.New(v.Type().Elem()).Elem()
		elem.Set(v.MapIndex(key))
		mutateHashLeaf(t, elem, steps[1:])
		cp := reflect.MakeMap(v.Type())
		cp.SetMapIndex(key, elem)
		v.Set(cp)
	default:
		t.Fatalf("cannot step into %s", v.Kind())
	}
}

// filledService returns a Service with every hashed scalar set, and the
// leaves the walk found.
func filledService(t *testing.T) (*Service, []hashLeaf) {
	t.Helper()
	s := &Service{}
	w := &hashWalker{t: t, classify: true}
	w.walk(reflect.ValueOf(s).Elem(), "", nil)
	return s, w.leaves
}

// TestServiceHash_FieldClassification is the guard for the template/desired
// split. A field in the wrong half either replaces every instance for a
// change no container sees, or never reaches running instances at all; both
// are silent. Every scalar reachable from Service is changed on its own and
// must move exactly the hashes its classification says. A new top-level
// field fails here until it is added to serviceHashFields.
func TestServiceHash_FieldClassification(t *testing.T) {
	base, leaves := filledService(t)
	baseFull, baseTmpl := base.CalculateHash(), base.CalculateTemplateHash()

	used := map[string]bool{}
	for _, leaf := range leaves {
		class, prefix, ok := classifyHashPath(leaf.path)
		if !ok {
			t.Errorf("%s is not classified: add it to serviceHashFields as template, desired or none", leaf.path)
			continue
		}
		used[prefix] = true

		cp := *base // mutateHashLeaf copies everything it passes through
		s := &cp
		mutateHashLeaf(t, reflect.ValueOf(s).Elem(), leaf.steps)
		movedFull := s.CalculateHash() != baseFull
		movedTmpl := s.CalculateTemplateHash() != baseTmpl

		switch class {
		case hashTemplate:
			assert.True(t, movedTmpl, "%s is template but did not move the template hash", leaf.path)
			assert.True(t, movedFull, "%s is template but did not move the full hash", leaf.path)
		case hashDesired:
			assert.False(t, movedTmpl, "%s is desired state but moved the template hash", leaf.path)
			assert.True(t, movedFull, "%s is desired state but did not move the full hash", leaf.path)
		case hashNone:
			assert.False(t, movedTmpl, "%s is unhashed but moved the template hash", leaf.path)
			assert.False(t, movedFull, "%s is unhashed but moved the full hash", leaf.path)
		}
	}

	var stale []string
	for prefix := range serviceHashFields {
		if !used[prefix] {
			stale = append(stale, prefix)
		}
	}
	sort.Strings(stale)
	assert.Empty(t, stale, "serviceHashFields names fields Service no longer has")
}

// The store round-trips a service through JSON, which turns empty slices and
// maps into nil. A spec compared against its stored copy must not look
// changed, or every cast would roll the fleet.
func TestServiceHash_StableAcrossStoreRoundTrip(t *testing.T) {
	base, _ := filledService(t)
	base.Args = []string{}
	base.Env = map[string]string{}
	base.InitSteps = []InitStep{}
	base.Labels = map[string]string{}

	raw, err := json.Marshal(base)
	require.NoError(t, err)
	var stored Service
	require.NoError(t, json.Unmarshal(raw, &stored))

	assert.Equal(t, base.CalculateTemplateHash(), stored.CalculateTemplateHash())
	assert.Equal(t, base.CalculateHash(), stored.CalculateHash())
}

func TestServiceHash_VIPIsNotHashed(t *testing.T) {
	a := &Service{Image: "app:v1"}
	b := &Service{Image: "app:v1", Discovery: &ServiceDiscovery{VIP: "10.0.0.7"}}
	assert.Equal(t, a.CalculateHash(), b.CalculateHash(),
		"a rendered spec has no VIP; the stored one does")
}

func TestServiceHash_GPU(t *testing.T) {
	tmpl := func(g *GPURequest) string {
		return (&Service{Image: "app:v1", Resources: Resources{GPU: g}}).CalculateTemplateHash()
	}

	assert.NotEqual(t, tmpl(nil), tmpl(&GPURequest{}),
		"no request and one whole device are different containers")
	assert.Equal(t, tmpl(&GPURequest{}), tmpl(&GPURequest{Count: 1}),
		"spelling out the implicit count must not replace anything")
	assert.NotEqual(t, tmpl(&GPURequest{Count: 1}), tmpl(&GPURequest{Count: 2}))
	assert.NotEqual(t, tmpl(&GPURequest{VRAM: "20Gi"}), tmpl(&GPURequest{VRAM: "40Gi"}),
		"a same-shape vram resize changes the device assignment")
	assert.NotEqual(t, tmpl(&GPURequest{Count: 2}), tmpl(&GPURequest{Count: 2, AllowHeterogeneous: true}))
}

// The cases the walk cannot see because they are about values, not fields.
func TestServiceHash_TemplateValueCases(t *testing.T) {
	tmpl := func(mut func(*Service)) string {
		s := &Service{Image: "app:v1"}
		mut(s)
		return s.CalculateTemplateHash()
	}
	none := func(*Service) {}

	cases := map[string]func(*Service){
		"privileged":     func(s *Service) { s.SecurityContext = &SecurityContext{Privileged: true} },
		"capAdd":         func(s *Service) { s.SecurityContext = &SecurityContext{CapAdd: []string{"SYS_ADMIN"}} },
		"hostPort":       func(s *Service) { s.Ports = []ServicePort{{Name: "http", Port: 80, HostPort: 8080}} },
		"envFrom":        func(s *Service) { s.EnvFrom = []EnvFromSource{{SecretName: "db"}} },
		"initStep image": func(s *Service) { s.InitSteps = []InitStep{{Name: "migrate", Image: "app:v1", Command: "migrate"}} },
	}
	for name, mut := range cases {
		assert.NotEqual(t, tmpl(none), tmpl(mut), "%s must replace instances", name)
	}

	for name, mut := range map[string]func(*Service){
		"registry password": func(s *Service) {
			s.Registry = &ServiceRegistryOverride{Auth: &RegistryAuth{Type: "basic", Password: "rotated"}}
		},
		"label":          func(s *Service) { s.Labels = map[string]string{"team": "a"} },
		"expose host":    func(s *Service) { s.Expose = &ServiceExpose{Port: "http", Host: "a.example.com"} },
		"network policy": func(s *Service) { s.NetworkPolicy = &ServiceNetworkPolicy{} },
	} {
		assert.Equal(t, tmpl(none), tmpl(mut), "%s must not replace instances", name)
	}
}

func TestServiceHash_ClaimTemplateOnlyAffectsNewOrdinals(t *testing.T) {
	withTemplate := func(size string) *Service {
		return &Service{Image: "app:v1", Volumes: []VolumeMount{{Name: "data", MountPath: "/data",
			ClaimTemplate: &VolumeClaimTemplate{Size: size}}}}
	}
	a, b := withTemplate("10Gi"), withTemplate("20Gi")
	assert.Equal(t, a.CalculateTemplateHash(), b.CalculateTemplateHash(),
		"a replacement reattaches the existing volume, so a resize must not roll")
	assert.NotEqual(t, a.CalculateHash(), b.CalculateHash())

	claim := &Service{Image: "app:v1", Volumes: []VolumeMount{{Name: "data", MountPath: "/data",
		Claim: &VolumeClaim{Name: "data"}}}}
	assert.NotEqual(t, a.CalculateTemplateHash(), claim.CalculateTemplateHash(),
		"switching a mount between a claim and a claim template changes what is mounted")
}

func TestServiceHash_DependenciesOrderInsensitive(t *testing.T) {
	a := &Service{Image: "app:v1", Dependencies: []DependencyRef{
		{Service: "db", Namespace: "default"}, {Secret: "creds", Namespace: "default"}, {Service: "cache", Namespace: "other"},
	}}
	b := &Service{Image: "app:v1", Dependencies: []DependencyRef{
		{Service: "cache", Namespace: "other"}, {Secret: "creds", Namespace: "default"}, {Service: "db", Namespace: "default"},
	}}
	assert.Equal(t, a.CalculateHash(), b.CalculateHash())
}
