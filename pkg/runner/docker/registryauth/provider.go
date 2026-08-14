package registryauth

import (
	"context"
	"strings"
)

// Provider supplies Docker RegistryAuth for a given image.
//
// Match receives both the registry host and the full image reference so a
// provider can scope itself to a repository PATH, not just a host. That
// distinction matters on shared registries: a credential registered for
// ghcr.io used to be attached to every ghcr.io pull, including public
// repositories needing no auth at all — so an expired token broke images
// that would have pulled fine anonymously.
type Provider interface {
	Match(host string, imageRef string) bool
	Resolve(ctx context.Context, host string, imageRef string) (string, error)
}

// Scoped is an OPTIONAL capability. Providers built from an explicit registry
// pattern implement it so the resolver can rank candidates and prefer the most
// specific match (ghcr.io/myorg/app beats ghcr.io/myorg beats ghcr.io) — the
// same precedence Kubernetes' credential keyring applies. Providers with no
// configured pattern (ambient docker-config / metadata credentials) don't
// implement it and always rank last.
type Scoped interface {
	Pattern() string
}

// Anonymous is an OPTIONAL capability marking a pattern as deliberately
// credential-free. Combined with most-specific-wins, a narrow anonymous entry
// overrides a broader credentialed one — how an operator says "this repo on my
// otherwise-private registry is public, never send a token for it".
type Anonymous interface {
	IsAnonymous() bool
}

// simple wildcard matcher: supports leading '*.' or '*'
func hostMatches(pattern, host string) bool {
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(pattern, host)
	}
	idx := strings.Index(pattern, "*")
	suffix := pattern[idx+1:]
	return strings.HasSuffix(host, suffix)
}

// splitPattern divides a configured registry pattern into its host and
// optional repository-path prefix: "ghcr.io/myorg" -> ("ghcr.io", "myorg").
func splitPattern(pattern string) (host, path string) {
	pattern = strings.Trim(pattern, "/")
	if i := strings.Index(pattern, "/"); i >= 0 {
		return pattern[:i], strings.Trim(pattern[i+1:], "/")
	}
	return pattern, ""
}

// imageRepoPath returns an image reference's repository path with the registry
// host, tag and digest stripped:
//
//	ghcr.io/floruntime/flo:0.1.0 -> floruntime/flo
//	myorg/app@sha256:...         -> myorg/app
//	nginx:alpine                 -> nginx
func imageRepoPath(imageRef string) string {
	ref := imageRef
	// Strip the digest first: everything from '@' onwards.
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	// Strip the tag — the last ':' AFTER the final '/', so a host:port
	// prefix (localhost:5000/app) isn't mistaken for a tag.
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	// Drop the registry host when the first segment looks like one.
	if i := strings.Index(ref, "/"); i >= 0 {
		first := ref[:i]
		if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
			return strings.Trim(ref[i+1:], "/")
		}
	}
	return strings.Trim(ref, "/")
}

// patternMatches reports whether a configured registry pattern applies to an
// image. A host-only pattern ("ghcr.io", "*.pkg.dev") matches every image on
// that host — the historical behaviour, preserved. A pattern carrying a path
// prefix ("ghcr.io/myorg") matches only repositories under it.
//
// The path comparison is segment-aware: "ghcr.io/myorg" matches
// ghcr.io/myorg/app but NOT ghcr.io/myorg-evil/app.
func patternMatches(pattern, host, imageRef string) bool {
	patHost, patPath := splitPattern(pattern)
	if !hostMatches(patHost, host) {
		return false
	}
	if patPath == "" {
		return true // host-wide
	}
	imgPath := imageRepoPath(imageRef)
	if imgPath == patPath {
		return true
	}
	return strings.HasPrefix(imgPath, patPath+"/")
}

// patternRank scores a pattern's specificity. Path depth dominates (a
// repository-scoped entry always beats a host-wide one), then an exact host
// beats a wildcard, then raw length as a stable tie-break.
func patternRank(pattern string) (depth int, exactHost bool, length int) {
	patHost, patPath := splitPattern(pattern)
	if patPath != "" {
		depth = len(strings.Split(patPath, "/"))
	}
	return depth, !strings.Contains(patHost, "*"), len(pattern)
}

// MoreSpecific reports whether pattern a is a more specific match than b.
func MoreSpecific(a, b string) bool {
	aDepth, aExact, aLen := patternRank(a)
	bDepth, bExact, bLen := patternRank(b)
	if aDepth != bDepth {
		return aDepth > bDepth
	}
	if aExact != bExact {
		return aExact
	}
	return aLen > bLen
}
