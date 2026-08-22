// Package docker — Registry auth + image pulls: provider chain, wildcard
// scoping, pull-error annotation. Split from runner.go (RUNE-312).
package docker

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	imageTypes "github.com/docker/docker/api/types/image"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner/docker/registryauth"
	runetypes "github.com/runestack/rune/pkg/types"
)

// pullImage pulls an image from the registry, honoring the supplied
// imagePull mode ("always", "missing", "never"). Empty defaults to
// "always". For "never", no pull is attempted; container creation will
// fail later if the image is missing locally.
func (r *DockerRunner) pullImage(ctx context.Context, image string, policy string, anonymous bool) error {
	switch policy {
	case runetypes.ImagePullNever:
		r.logger.Debug("Skipping image pull (imagePull=never)", log.Str("image", image))
		return nil
	case runetypes.ImagePullMissing:
		if _, _, err := r.client.ImageInspectWithRaw(ctx, image); err == nil {
			r.logger.Debug("Image present locally; skipping pull (imagePull=missing)", log.Str("image", image))
			return nil
		}
	case runetypes.ImagePullAlways, "":
		// fall through and re-pull every time
	default:
		// Unknown values are treated as always but logged.
		r.logger.Warn("Unknown imagePull value; defaulting to always",
			log.Str("imagePull", policy), log.Str("image", image))
	}

	r.logger.Info("Pulling Docker image",
		log.Str("image", image), log.Str("policy", policy))

	// Resolve registry auth for this image if configured. imagePullAnonymous
	// short-circuits it entirely: the service author has declared this image
	// public, so no credential is sent even if a configured entry matches its
	// registry. Sending a credential that a public repo doesn't need is how an
	// expired token takes down an image that would pull fine anonymously.
	host := parseImageHost(image)
	registryAuth := ""
	if anonymous {
		r.logger.Debug("Pulling anonymously (imagePullAnonymous)",
			log.Str("image", image), log.Str("host", host))
	} else {
		registryAuth = r.resolveRegistryAuth(image)
	}
	if registryAuth == "" {
		r.logger.Debug("No registry auth resolved for image",
			log.Str("image", image),
			log.Str("host", host))
	} else {
		r.logger.Debug("Resolved registry auth for image",
			log.Str("image", image),
			log.Str("host", host))
	}

	// Pull the image
	reader, err := r.client.ImagePull(ctx, image, imageTypes.PullOptions{RegistryAuth: registryAuth})
	if err != nil {
		return r.annotatePullError(image, registryAuth != "", err)
	}
	defer reader.Close()

	// Read the output to complete the pull. Registry-side failures (including
	// auth rejections) surface here rather than from ImagePull itself, because
	// the daemon streams them back as part of the pull output.
	if _, err = io.Copy(io.Discard, reader); err != nil {
		return r.annotatePullError(image, registryAuth != "", err)
	}
	return nil
}

// annotatePullError enriches an authentication rejection with the context an
// operator needs to act. A bare "denied" from the registry gives no indication
// that a CONFIGURED CREDENTIAL was involved at all — during one incident a
// public image failed to pull because a host-wide credential had expired, and
// the error read as though the image itself were private. Naming the pattern
// that supplied the credential points straight at the entry (and therefore the
// secret) to fix, and at the alternative of scoping it more narrowly.
func (r *DockerRunner) annotatePullError(image string, usedAuth bool, err error) error {
	if err == nil || !isRegistryAuthError(err) {
		return err
	}
	if !usedAuth {
		return fmt.Errorf("%w (registry requires credentials; no [[docker.registries]] entry matches %s)",
			err, image)
	}
	pattern := r.authPatternFor(image)
	if pattern == "" {
		pattern = parseImageHost(image)
	}
	r.logOrDefault().Warn("Registry rejected the configured credential",
		log.Str("image", image),
		log.Str("registry_pattern", pattern),
		log.Err(err))
	return fmt.Errorf("%w (the credential configured for registry pattern %q was rejected; "+
		"renew it, or if this image is public scope the pattern to your private repositories "+
		"or mark it anonymous)", err, pattern)
}

// isRegistryAuthError reports whether a pull failure is a credential
// rejection rather than a transient/network problem. Docker surfaces these as
// message text, so matching is by substring.
func isRegistryAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "denied") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "authentication required") ||
		strings.Contains(msg, "forbidden")
}

// registryProviders returns the registry-auth chain, building it on first
// call and reusing it thereafter.
//
// The build must happen exactly once even when several pulls race into
// it: the reconciler dispatches one goroutine per service key, so two
// services starting together both reach this on a cold runner. It is
// also the only correct place for the "no providers" case to be
// distinguished from "not built yet" — the previous nil-check idiom
// conflated them and needed a sentinel to compensate.
func (r *DockerRunner) registryProviders() []registryauth.Provider {
	r.providersOnce.Do(func() {
		var regs []map[string]any
		for _, rc := range r.config.Registries {
			regs = append(regs, map[string]any{
				"registry": rc.Registry,
				"auth": map[string]any{
					"type":     rc.Auth.Type,
					"username": rc.Auth.Username,
					"password": rc.Auth.Password,
					"token":    rc.Auth.Token,
					"region":   rc.Auth.Region,
				},
			})
		}
		providers := registryauth.BuildProviders(context.Background(), regs)
		// Ambient fallbacks (GCE metadata SA for *.pkg.dev/gcr.io,
		// then the docker CLI config of the runed user) go after all
		// configured providers so explicit config always wins. See
		// issue #144 — without these, private pulls on a node whose
		// SA has artifactregistry.reader still went out anonymous.
		if !r.config.DisableAmbientRegistryAuth {
			providers = append(providers, registryauth.AmbientProviders(context.Background())...)
		}
		// Published only after it is fully built, so no caller can observe
		// a partial chain.
		r.providers = providers
	})
	return r.providers
}

// resolveRegistryAuth selects an auth entry based on image host and encodes it for Docker ImagePull
func (r *DockerRunner) resolveRegistryAuth(imageRef string) string {
	host := parseImageHost(imageRef)
	if host == "" {
		return ""
	}
	providers := r.registryProviders()
	// Collect every provider that claims this image, then prefer the MOST
	// SPECIFIC one. Previously this was first-match on the host alone, so a
	// credential registered for a whole registry was attached to every image
	// on it — including public repositories that need no auth, which turned an
	// expired token into a hard failure for images that would pull anonymously.
	// Repository-scoped patterns now win over host-wide ones (the precedence
	// Kubernetes' credential keyring uses); ambient providers carry no pattern
	// and rank last, preserving "explicit config wins".
	type candidate struct {
		p       registryauth.Provider
		pattern string
		scoped  bool
	}
	var candidates []candidate
	for _, p := range providers {
		if !p.Match(host, imageRef) {
			continue
		}
		c := candidate{p: p}
		if s, ok := p.(registryauth.Scoped); ok {
			if pat := s.Pattern(); pat != "" {
				c.pattern, c.scoped = pat, true
			}
		}
		candidates = append(candidates, c)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		// Unscoped (ambient) providers always sort last; among scoped ones,
		// the more specific pattern wins.
		if candidates[i].scoped != candidates[j].scoped {
			return candidates[i].scoped
		}
		if !candidates[i].scoped {
			return false // preserve configured order among ambient providers
		}
		return registryauth.MoreSpecific(candidates[i].pattern, candidates[j].pattern)
	})

	if len(candidates) == 0 && len(r.config.Registries) > 0 {
		// Nothing matched, yet registries ARE configured. Most often a
		// pattern that can never match anything — e.g. before path-scoped
		// matching existed, `registry = "ghcr.io/myorg"` parsed fine and
		// then silently matched nothing, so the credential looked configured
		// but was never used. Say so rather than pulling anonymously in
		// silence.
		r.logOrDefault().Debug("No configured registry matched image; pulling anonymously",
			log.Str("image", imageRef), log.Str("host", host))
	}

	for _, c := range candidates {
		// An explicitly anonymous entry short-circuits: the operator has
		// declared this repository public, so no credential is sent and no
		// broader entry gets a chance to attach one.
		if a, ok := c.p.(registryauth.Anonymous); ok && a.IsAnonymous() {
			r.logOrDefault().Debug("Pulling anonymously: registry pattern is marked anonymous",
				log.Str("image", imageRef), log.Str("pattern", c.pattern))
			return ""
		}
		if auth, _ := c.p.Resolve(context.Background(), host, imageRef); auth != "" {
			r.lastAuthPattern.Store(imageRef, c.pattern)
			return auth
		}
	}
	return ""
}

// authPatternFor reports which configured registry pattern supplied the
// credential for the last pull attempt of imageRef, if any. Used to name the
// offending entry when a registry rejects the credential — a bare "denied"
// gives an operator no indication a *configured credential* was even involved.
func (r *DockerRunner) authPatternFor(imageRef string) string {
	if v, ok := r.lastAuthPattern.Load(imageRef); ok {
		if s, _ := v.(string); s != "" {
			return s
		}
	}
	return ""
}

func parseImageHost(imageRef string) string {
	// Format examples:
	// ghcr.io/owner/repo:tag
	// 123456789012.dkr.ecr.us-east-1.amazonaws.com/repo:tag
	// nginx:alpine (Docker Hub)
	// If no '/' present, it's a library image on Docker Hub
	if !strings.Contains(imageRef, "/") {
		return "index.docker.io"
	}
	parts := strings.Split(imageRef, "/")
	first := parts[0]
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		return first
	}
	// registry not explicit -> Docker Hub
	return "index.docker.io"
}
