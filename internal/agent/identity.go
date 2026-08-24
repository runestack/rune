package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/runestack/rune/pkg/utils"
)

// LoadOrCreateIdentity reads the agent identity from <dir>/node-identity.json,
// or creates and persists a new one if the file does not exist.
//
// The persisted file ensures NodeID is stable across restarts. We never
// regenerate NodeID once written; if the file exists but is malformed,
// LoadOrCreateIdentity returns an error rather than silently overwriting
// it (operator must intervene).
//
// preferredName, when non-empty and DNS-1123 valid, is the NodeID for a
// NEW identity (`runed --node-name`). Otherwise a valid hostname is used,
// falling back to node-<hex>. Minting only ever affects first boot: an
// existing file is returned verbatim, because Volume.BoundNode rows on
// disk already carry whatever ID was minted then and renaming in place
// would unmount every volume on the next restart.
func LoadOrCreateIdentity(dir string, preferredName string) (Identity, error) {
	if dir == "" {
		return Identity{}, errors.New("identity: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Identity{}, fmt.Errorf("identity: mkdir %q: %w", dir, err)
	}
	path := filepath.Join(dir, "node-identity.json")

	if data, err := os.ReadFile(path); err == nil {
		var id Identity
		if err := json.Unmarshal(data, &id); err != nil {
			return Identity{}, fmt.Errorf("identity: parse %q: %w", path, err)
		}
		if id.NodeID == "" {
			return Identity{}, fmt.Errorf("identity: empty NodeID in %q", path)
		}
		return id, nil
	} else if !os.IsNotExist(err) {
		return Identity{}, fmt.Errorf("identity: read %q: %w", path, err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	id := Identity{
		NodeID:   mintNodeID(preferredName, hostname),
		Hostname: hostname,
	}
	out, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return Identity{}, fmt.Errorf("identity: marshal: %w", err)
	}
	// Atomic write: write to .tmp then rename.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return Identity{}, fmt.Errorf("identity: write %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return Identity{}, fmt.Errorf("identity: rename %q: %w", path, err)
	}
	return id, nil
}

// mintNodeID picks the NodeID for a brand-new identity: an explicit
// --node-name first, then the hostname, then a random hex fallback. The
// first two are only taken when they are valid DNS-1123 labels, because
// the ID is used as a store key, a stream label and (for the DigitalOcean
// volume driver) a droplet-name lookup.
func mintNodeID(preferredName, hostname string) string {
	for _, candidate := range []string{preferredName, hostname} {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "" {
			continue
		}
		if err := utils.ValidateDNS1123Name(candidate); err != nil {
			continue
		}
		return candidate
	}
	return "node-" + randomHex(8)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Should never happen on supported OSes; fall back to a static
		// value rather than panicking. The caller will still get a
		// usable (but non-unique) NodeID and the operator can fix it.
		for i := range b {
			b[i] = byte(i)
		}
	}
	return hex.EncodeToString(b)
}
