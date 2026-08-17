package docker

import (
	"sync"
	"testing"

	"github.com/runestack/rune/pkg/log"
)

// TestRegistryProvidersConcurrentFirstUse is the regression for issue
// #188: the registry-auth chain was built under a bare
// `if r.providers == nil` check on a shared *DockerRunner.
//
// Pulls run on the reconciler's worker pool — reconcileWorkerCount
// goroutines, dispatched one per service key — so on a cold runner two
// services starting together both reach first-use at the same moment.
// That is an unsynchronised read/write of a slice field, and because the
// build was never repeated, whatever a racing goroutine published became
// the value every later pull used.
//
// Run under -race, this fails on the pre-#188 code and passes on the fix.
// It also asserts the outcome, not just the absence of a race: every
// caller must observe the same fully-built chain and resolve the same
// credential.
func TestRegistryProvidersConcurrentFirstUse(t *testing.T) {
	r := newAuthTestRunner(t,
		RegistryConfig{
			Name:     "ghcr",
			Registry: "ghcr.io/withpropeller",
			Auth:     RegistryAuth{Type: "basic", Username: "scoped-user", Password: "pw"},
		},
		RegistryConfig{
			Name:     "ghcr-wide",
			Registry: "ghcr.io",
			Auth:     RegistryAuth{Type: "basic", Username: "host-user", Password: "pw"},
		},
	)

	const goroutines = 16
	var wg sync.WaitGroup
	results := make([]string, goroutines)
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together, to maximise overlap on first use
			results[i] = decodedUser(t, r.resolveRegistryAuth("ghcr.io/withpropeller/api:v1"))
		}(i)
	}
	close(start)
	wg.Wait()

	// The path-scoped entry must win for every caller. A torn or
	// partially-built chain would show up as a different user (the
	// host-wide entry) or as an empty result.
	for i, got := range results {
		if got != "scoped-user" {
			t.Errorf("goroutine %d resolved %q, want %q — the provider chain was not observed fully built",
				i, got, "scoped-user")
		}
	}
}

// TestRegistryProvidersBuiltOnce: the chain is built on first use and
// reused. Rebuilding per pull would re-probe the GCE metadata service and
// the docker CLI config on every image pull.
func TestRegistryProvidersBuiltOnce(t *testing.T) {
	r := &DockerRunner{
		logger: log.NewLogger(),
		config: func() *DockerConfig {
			c := DefaultDockerConfig()
			c.DisableAmbientRegistryAuth = true
			c.Registries = []RegistryConfig{{
				Name:     "ghcr",
				Registry: "ghcr.io",
				Auth:     RegistryAuth{Type: "basic", Username: "u", Password: "p"},
			}}
			return c
		}(),
	}

	first := r.registryProviders()
	second := r.registryProviders()
	if len(first) == 0 {
		t.Fatal("expected at least one provider from the configured registry")
	}
	if &first[0] != &second[0] {
		t.Error("registryProviders rebuilt the chain instead of reusing it")
	}
}

// TestRegistryProvidersNoConfigIsStable covers the case the old sentinel
// existed for: with nothing configured the chain is legitimately empty,
// and that must not be mistaken for "not built yet" and rebuilt forever.
func TestRegistryProvidersNoConfigIsStable(t *testing.T) {
	r := newAuthTestRunner(t) // no registries, ambient disabled

	if got := r.registryProviders(); len(got) != 0 {
		t.Fatalf("expected an empty chain, got %d providers", len(got))
	}
	// Second call must not panic or rebuild; an empty result is a real
	// answer, not a missing one.
	if got := r.registryProviders(); len(got) != 0 {
		t.Fatalf("expected an empty chain on reuse, got %d providers", len(got))
	}
	if auth := r.resolveRegistryAuth("ghcr.io/anyone/img:v1"); auth != "" {
		t.Errorf("expected no credential with nothing configured, got %q", auth)
	}
}
