// Package proto is the host <-> guest agent wire format for the RUNE-322
// spike: each frame is a 4-byte big-endian length followed by a JSON body.
// It is a draft of the RUNE-322 §4.1 agent protocol, not a stable API.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// AgentPort is the vsock port the guest agent listens on.
const AgentPort = 1024

// MaxFrame bounds a single frame so a bad peer cannot make us allocate
// arbitrary memory.
const MaxFrame = 16 << 20

// Ops understood by the agent.
const (
	OpPing      = "ping"
	OpExec      = "exec"
	OpReadFile  = "read_file"
	OpWriteFile = "write_file"
	OpCloned    = "cloned"
	OpIdentity  = "identity"
	OpMemInfo   = "meminfo"
)

type Request struct {
	ID    uint64     `json:"id"`
	Op    string     `json:"op"`
	Exec  *ExecArgs  `json:"exec,omitempty"`
	File  *FileArgs  `json:"file,omitempty"`
	Clone *CloneArgs `json:"clone,omitempty"`
}

type ExecArgs struct {
	Argv      []string `json:"argv"`
	Env       []string `json:"env,omitempty"`
	Dir       string   `json:"dir,omitempty"`
	Stdin     []byte   `json:"stdin,omitempty"`
	TimeoutMS int64    `json:"timeout_ms,omitempty"`
}

type FileArgs struct {
	Path string `json:"path"`
	Data []byte `json:"data,omitempty"`
	Mode uint32 `json:"mode,omitempty"`
}

// CloneArgs is sent once, right after a sandbox is restored from a shared
// snapshot, so that siblings stop sharing identity and RNG state.
type CloneArgs struct {
	Hostname   string `json:"hostname"`
	Entropy    []byte `json:"entropy"`
	UnixNanos  int64  `json:"unix_nanos"`
	RegenSSH   bool   `json:"regen_ssh"`
	NewMachine bool   `json:"new_machine_id"`
}

type Response struct {
	ID       uint64      `json:"id"`
	OK       bool        `json:"ok"`
	Error    string      `json:"error,omitempty"`
	Exec     *ExecResult `json:"exec,omitempty"`
	Data     []byte      `json:"data,omitempty"`
	Identity *Identity   `json:"identity,omitempty"`
	Mem      *MemInfo    `json:"mem,omitempty"`
}

type ExecResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     []byte `json:"stdout,omitempty"`
	Stderr     []byte `json:"stderr,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	DurationNS int64  `json:"duration_ns"`
}

// Identity is what E3 compares across siblings restored from one snapshot.
type Identity struct {
	MachineID   string `json:"machine_id"`
	Hostname    string `json:"hostname"`
	BootID      string `json:"boot_id"`
	SSHHostKey  string `json:"ssh_host_key_sha256"`
	URandom     string `json:"urandom_hex"`
	UptimeSec   string `json:"uptime"`
	GuestTimeNS int64  `json:"guest_time_ns"`
}

// MemInfo is a subset of /proc/meminfo, in KiB.
type MemInfo struct {
	TotalKiB     uint64 `json:"total_kib"`
	FreeKiB      uint64 `json:"free_kib"`
	AvailableKiB uint64 `json:"available_kib"`
	CachedKiB    uint64 `json:"cached_kib"`
}

func Write(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return fmt.Errorf("frame too large: %d bytes", len(b))
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

func Read(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return fmt.Errorf("frame too large: %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
