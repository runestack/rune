package upgrade

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
)

// Checksums maps asset basenames to hex sha256 digests.
//
// The release workflow writes lines as `sha256sum dist/*.tar.gz`, so paths
// carry a `dist/` prefix; keys here are basenames so lookups work with the
// bare asset name. The shell installers survive the prefix via substring
// grep — a Go parser matching the full path exactly would find nothing. A
// missing digest is a hard failure here, never a silent skip: scripts/install.sh
// skips verification when the digest is absent, and that is the bug, not the
// precedent.
type Checksums map[string]string

// FetchChecksums downloads and parses checksums.txt for a release tag.
func FetchChecksums(ctx context.Context, hc *http.Client, tag string) (Checksums, error) {
	resp, err := fetchWithRetry(ctx, hc, DownloadURL(tag, ChecksumsAsset))
	if err != nil {
		return nil, fmt.Errorf("fetching %s for %s: %w", ChecksumsAsset, tag, err)
	}
	defer resp.Body.Close()
	return ParseChecksums(io.LimitReader(resp.Body, 1<<20))
}

// ParseChecksums parses sha256sum output: `<hex><space><space-or-*><path>`.
func ParseChecksums(r io.Reader) (Checksums, error) {
	cs := Checksums{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, fmt.Errorf("malformed checksums line: %q", line)
		}
		name := path.Base(strings.TrimPrefix(fields[1], "*"))
		cs[name] = strings.ToLower(fields[0])
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, fmt.Errorf("checksums file is empty")
	}
	return cs, nil
}

// Digest returns the digest for an asset, erroring when absent.
func (c Checksums) Digest(asset string) (string, error) {
	d, ok := c[asset]
	if !ok {
		return "", fmt.Errorf("no digest for %s in %s", asset, ChecksumsAsset)
	}
	return d, nil
}

// SHA256File hashes a file on disk.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return SHA256Reader(f)
}

// SHA256Reader hashes a stream.
func SHA256Reader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyDigest compares two hex digests case-insensitively.
func VerifyDigest(got, want string) error {
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}
