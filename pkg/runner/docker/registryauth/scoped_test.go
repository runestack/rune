package registryauth

import "testing"

// TestPatternMatches_PathScoping covers the core of #178: a credential must be
// attachable to a repository prefix, not just a whole registry host.
//
// The motivating incident: a host-wide `ghcr.io` credential expired, and
// ghcr.io/floruntime/flo — a PUBLIC image needing no auth — started failing
// with "denied", because the dead token was attached to its pull.
func TestPatternMatches_PathScoping(t *testing.T) {
	cases := []struct {
		name     string
		pattern  string
		host     string
		imageRef string
		want     bool
	}{
		// Host-wide patterns keep their historical behaviour.
		{"host-wide matches any repo", "ghcr.io", "ghcr.io", "ghcr.io/floruntime/flo:0.1.0", true},
		{"host-wide matches own org", "ghcr.io", "ghcr.io", "ghcr.io/myorg/app:v1", true},
		{"different host never matches", "ghcr.io", "docker.io", "docker.io/library/nginx:alpine", false},

		// The fix: a path-scoped pattern only claims its own repositories.
		{"scoped matches under prefix", "ghcr.io/myorg", "ghcr.io", "ghcr.io/myorg/app:v1", true},
		{"scoped matches prefix exactly", "ghcr.io/myorg/app", "ghcr.io", "ghcr.io/myorg/app:v1", true},
		{"scoped does NOT claim another org", "ghcr.io/myorg", "ghcr.io", "ghcr.io/floruntime/flo:0.1.0", false},

		// Segment-aware: a prefix must not match a longer sibling name.
		{"scoped is segment-aware", "ghcr.io/myorg", "ghcr.io", "ghcr.io/myorg-evil/app:v1", false},

		// Host wildcards keep working, with and without a path.
		{"wildcard host", "*.pkg.dev", "europe-west2-docker.pkg.dev", "europe-west2-docker.pkg.dev/p/a:v1", true},
		{"wildcard host + path", "*.pkg.dev/p", "europe-west2-docker.pkg.dev", "europe-west2-docker.pkg.dev/p/a:v1", true},
		{"wildcard host + wrong path", "*.pkg.dev/other", "europe-west2-docker.pkg.dev", "europe-west2-docker.pkg.dev/p/a:v1", false},

		// Docker Hub style refs (no explicit host in the reference).
		{"implicit hub host, scoped", "index.docker.io/myorg", "index.docker.io", "myorg/app:v1", true},
		{"implicit hub host, other org", "index.docker.io/myorg", "index.docker.io", "otherorg/app:v1", false},

		// Digest-pinned references must scope identically to tagged ones.
		{"digest ref scoped", "ghcr.io/myorg", "ghcr.io", "ghcr.io/myorg/app@sha256:abc123", true},
		{"digest ref other org", "ghcr.io/myorg", "ghcr.io", "ghcr.io/other/app@sha256:abc123", false},

		// A registry with an explicit port must not have it read as a tag.
		{"host:port with path", "localhost:5000/team", "localhost:5000", "localhost:5000/team/app:v1", true},
		{"host:port wrong path", "localhost:5000/team", "localhost:5000", "localhost:5000/other/app:v1", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := patternMatches(tc.pattern, tc.host, tc.imageRef); got != tc.want {
				t.Errorf("patternMatches(%q, %q, %q) = %v, want %v",
					tc.pattern, tc.host, tc.imageRef, got, tc.want)
			}
		})
	}
}

func TestImageRepoPath(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/floruntime/flo:0.1.0-dev.9":   "floruntime/flo",
		"ghcr.io/floruntime/flo":               "floruntime/flo",
		"ghcr.io/org/app@sha256:deadbeef":      "org/app",
		"myorg/app:v1":                         "myorg/app",
		"nginx:alpine":                         "nginx",
		"nginx":                                "nginx",
		"localhost:5000/team/app:v1":           "team/app",
		"europe-west2-docker.pkg.dev/p/r/i:v1": "p/r/i",
	}
	for in, want := range cases {
		if got := imageRepoPath(in); got != want {
			t.Errorf("imageRepoPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMoreSpecific pins the precedence rule: deeper paths win, then exact
// hosts over wildcards. This is what lets a narrow entry override a broad one.
func TestMoreSpecific(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"ghcr.io/myorg", "ghcr.io", true},           // path beats host-wide
		{"ghcr.io", "ghcr.io/myorg", false},          // and not the reverse
		{"ghcr.io/myorg/app", "ghcr.io/myorg", true}, // deeper path wins
		{"ghcr.io", "*.io", true},                    // exact host beats wildcard
		{"*.pkg.dev/p", "gcr.io", true},              // any path beats host-only
	}
	for _, tc := range cases {
		if got := MoreSpecific(tc.a, tc.b); got != tc.want {
			t.Errorf("MoreSpecific(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// A provider built from an anonymous entry must resolve to no credential —
// that's what makes a narrow "this repo is public" entry able to override a
// broader credentialed one.
func TestAnonymousProviderResolvesNoCredential(t *testing.T) {
	p := NewBasicTokenProvider(BasicTokenConfig{
		Registry:      "ghcr.io/floruntime",
		AnonymousOnly: true,
		Username:      "someone", // must be ignored
		Password:      "secret",
	})
	if !p.Match("ghcr.io", "ghcr.io/floruntime/flo:0.1.0") {
		t.Fatal("anonymous provider should still match its pattern")
	}
	if !p.IsAnonymous() {
		t.Fatal("IsAnonymous should report true")
	}
	auth, err := p.Resolve(t.Context(), "ghcr.io", "ghcr.io/floruntime/flo:0.1.0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if auth != "" {
		t.Fatalf("anonymous provider must resolve to no credential, got %q", auth)
	}
}
