// Package portforwarddaemon implements the detached daemon backing
// `rune port-forward -d`, plus the CLI-side RPC client and subcommands
// for `list` / `stop` / `logs`. See RUNE-123.
package portforwarddaemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// PortMapping is one [LOCAL:]REMOTE pair persisted in a forward's
// state file.
type PortMapping struct {
	Local  string `json:"local"`  // bind addr, e.g. "127.0.0.1:27017"
	Remote uint32 `json:"remote"` // remote port on the container
}

// TargetKind says whether the forward is bound to a service or a
// specific instance.
type TargetKind string

const (
	TargetService  TargetKind = "service"
	TargetInstance TargetKind = "instance"
)

// ForwardStatus mirrors the lifecycle column shown by `list`.
type ForwardStatus string

const (
	StatusActive          ForwardStatus = "active"
	StatusReconnecting    ForwardStatus = "reconnecting"
	StatusFailed          ForwardStatus = "failed"
	StatusUnauthenticated ForwardStatus = "unauthenticated"
)

// Forward is the persisted descriptor for one daemonized forward.
// In-memory state on the daemon is authoritative; this struct is what
// is written to ~/.rune/forwards/<id>.json and reported via `list`.
type Forward struct {
	ID          string        `json:"id"`
	Namespace   string        `json:"namespace"`
	TargetKind  TargetKind    `json:"target_kind"`
	Target      string        `json:"target"`
	InstancePin string        `json:"instance_pin,omitempty"`
	Mappings    []PortMapping `json:"mappings"`
	Context     string        `json:"context,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	Status      ForwardStatus `json:"status"`
	LastError   string        `json:"last_error,omitempty"`
}

// StateDir returns the directory holding daemon state for the current
// user, creating it with 0700 if needed.
func StateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	dir := filepath.Join(home, ".rune", "forwards")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// Tighten permissions in case it was created with a wider umask
	// previously.
	_ = os.Chmod(dir, 0o700)
	return dir, nil
}

// PidPath, SocketPath, LogPath return the canonical file paths under
// the state dir.
func PidPath(dir string) string    { return filepath.Join(dir, "daemon.pid") }
func SocketPath(dir string) string { return filepath.Join(dir, "daemon.sock") }
func LogPath(dir string) string    { return filepath.Join(dir, "daemon.log") }

// ForwardPath returns the JSON file path for a given forward id.
func ForwardPath(dir, id string) string {
	return filepath.Join(dir, id+".json")
}

// WriteForward persists a forward's descriptor. Atomic via tmpfile +
// rename so a partial write never leaves a corrupt state file behind.
func WriteForward(dir string, fwd *Forward) error {
	b, err := json.MarshalIndent(fwd, "", "  ")
	if err != nil {
		return err
	}
	final := ForwardPath(dir, fwd.ID)
	tmp, err := os.CreateTemp(dir, fwd.ID+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	_ = os.Chmod(tmpName, 0o600)
	return os.Rename(tmpName, final)
}

// RemoveForward removes the JSON descriptor for the given id (no-op if
// already gone).
func RemoveForward(dir, id string) error {
	err := os.Remove(ForwardPath(dir, id))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LoadForwards walks the state directory and returns every persisted
// descriptor, sorted by CreatedAt. Used by the daemon at startup
// (recovery) and as a fallback for `list` if the daemon is down.
func LoadForwards(dir string) ([]*Forward, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]*Forward, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !hasSuffix(name, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var fwd Forward
		if err := json.Unmarshal(b, &fwd); err != nil {
			continue
		}
		out = append(out, &fwd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}
