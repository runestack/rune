// Package upgrade implements RUNE-321: in-band upgrade of the Rune server
// and CLI. The CLI resolves a release and verifies checksums; runed stages
// its own upgrade as the unprivileged service user; a root systemd oneshot
// (`runed apply-upgrade`) independently re-verifies and applies it.
//
// The repository and download host are compile-time constants on purpose:
// a configurable upgrade URL is an arbitrary-code-execution knob.
package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// Repo is the canonical GitHub repository releases are fetched from.
	Repo = "runestack/rune"

	releasesAPIURL  = "https://api.github.com/repos/" + Repo + "/releases"
	downloadBaseURL = "https://github.com/" + Repo + "/releases/download"

	// ChecksumsAsset is the digest manifest published with every release.
	ChecksumsAsset = "checksums.txt"

	// maxArtifactBytes caps any artifact download. Real tarballs are
	// ~40-60 MB; the cap only has to stop a runaway response from
	// filling the disk (or the applier's tmpfs workdir).
	maxArtifactBytes = 512 << 20

	// downloadRetryBudget bounds retries on transient download failures.
	// GitHub's CDN edge near a server can 504 on release assets for a few
	// minutes right after publishing while other edges already serve them
	// (seen in production on dev.134) — so "cut a release, immediately
	// upgrade" must ride that out rather than fail.
	downloadRetryBudget = 4 * time.Minute
	downloadRetryDelay  = 15 * time.Second
)

// ServerAsset returns the server tarball name (rune + runed) for a linux
// architecture. runed ships linux-only.
func ServerAsset(arch string) string {
	return fmt.Sprintf("rune_linux_%s.tar.gz", arch)
}

// CLIAsset returns the CLI-only tarball name for an os/arch pair.
func CLIAsset(goos, arch string) string {
	return fmt.Sprintf("rune-cli_%s_%s.tar.gz", goos, arch)
}

// DownloadURL returns the release-asset URL for a tag.
func DownloadURL(tag, asset string) string {
	return fmt.Sprintf("%s/%s/%s", downloadBaseURL, tag, asset)
}

// githubRelease is the subset of the GitHub list-releases response we read.
type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

// ResolveNewest returns the tag of the newest release whose assets include
// checksums.txt and every name in required. It deliberately does not use
// GitHub's /releases/latest (which excludes prereleases — and every Rune
// release today is a prerelease), and it skips releases still missing
// assets, because release creation and asset upload are not atomic.
func ResolveNewest(ctx context.Context, hc *http.Client, required ...string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPIURL+"?per_page=10", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("listing releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("listing releases: GitHub returned %s", resp.Status)
	}
	var releases []githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&releases); err != nil {
		return "", fmt.Errorf("parsing release list: %w", err)
	}
	want := append([]string{ChecksumsAsset}, required...)
	for _, r := range releases {
		have := make(map[string]bool, len(r.Assets))
		for _, a := range r.Assets {
			have[a.Name] = true
		}
		complete := true
		for _, w := range want {
			if !have[w] {
				complete = false
				break
			}
		}
		if complete {
			return r.TagName, nil
		}
	}
	return "", fmt.Errorf("no release with complete assets among the newest %d", len(releases))
}

// fetchWithRetry GETs url, retrying transient failures (5xx, transport
// errors) within downloadRetryBudget. The caller owns closing the body of
// the returned response.
func fetchWithRetry(ctx context.Context, hc *http.Client, url string) (*http.Response, error) {
	deadline := time.Now().Add(downloadRetryBudget)
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := hc.Do(req)
		switch {
		case err != nil:
			lastErr = err
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		case resp.StatusCode >= 500:
			resp.Body.Close()
			lastErr = fmt.Errorf("GET %s: %s", url, resp.Status)
		default:
			// 4xx: the asset genuinely isn't there; retrying won't help.
			resp.Body.Close()
			return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("download did not succeed within %s: %w", downloadRetryBudget, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(downloadRetryDelay):
		}
	}
}
