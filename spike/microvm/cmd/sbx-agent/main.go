// sbx-agent is the guest agent for the RUNE-322 spike. It runs inside the
// microVM (started by systemd), listens on vsock, and answers framed JSON
// requests from the host: exec, file read/write, the post-clone identity
// reset, and identity/memory reports for the experiments.
//
// Build static: CGO_ENABLED=0 go build ./cmd/sbx-agent
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/runestack/rune/spike/microvm/internal/proto"
)

const maxOutput = 1 << 20

func main() {
	port := flag.Uint("port", proto.AgentPort, "vsock port to listen on")
	flag.Parse()
	log.SetFlags(log.Lmicroseconds)

	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		log.Fatalf("socket: %v", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: uint32(*port)}); err != nil {
		log.Fatalf("bind vsock port %d: %v", *port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("listening on vsock port %d", *port)
	for {
		nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.ECONNABORTED) {
				continue
			}
			log.Fatalf("accept: %v", err)
		}
		go serve(os.NewFile(uintptr(nfd), "vsock"))
	}
}

func serve(conn *os.File) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		var req proto.Request
		if err := proto.Read(r, &req); err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("read: %v", err)
			}
			return
		}
		resp := handle(&req)
		resp.ID = req.ID
		if err := proto.Write(conn, resp); err != nil {
			log.Printf("write: %v", err)
			return
		}
	}
}

func handle(req *proto.Request) *proto.Response {
	var (
		resp = &proto.Response{}
		err  error
	)
	switch req.Op {
	case proto.OpPing:
	case proto.OpExec:
		if req.Exec == nil || len(req.Exec.Argv) == 0 {
			err = errors.New("exec: argv required")
			break
		}
		resp.Exec, err = runExec(req.Exec)
	case proto.OpReadFile:
		if req.File == nil {
			err = errors.New("read_file: path required")
			break
		}
		resp.Data, err = os.ReadFile(req.File.Path)
	case proto.OpWriteFile:
		if req.File == nil {
			err = errors.New("write_file: path required")
			break
		}
		mode := os.FileMode(req.File.Mode)
		if mode == 0 {
			mode = 0o644
		}
		err = os.WriteFile(req.File.Path, req.File.Data, mode)
	case proto.OpCloned:
		if req.Clone == nil {
			err = errors.New("cloned: args required")
			break
		}
		err = cloned(req.Clone)
	case proto.OpIdentity:
		resp.Identity, err = identity()
	case proto.OpMemInfo:
		resp.Mem, err = meminfo()
	default:
		err = fmt.Errorf("unknown op %q", req.Op)
	}
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.OK = true
	}
	return resp
}

// limitedBuffer keeps the first maxOutput bytes. It deliberately does not
// embed bytes.Buffer: io.Copy would use the promoted ReadFrom and bypass the cap.
type limitedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxOutput - b.buf.Len(); len(p) > room {
		b.truncated = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func runExec(a *proto.ExecArgs) (*proto.ExecResult, error) {
	ctx := context.Background()
	if a.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(a.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, a.Argv[0], a.Argv[1:]...)
	cmd.Dir = a.Dir
	cmd.Env = append(os.Environ(), a.Env...)
	if len(a.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(a.Stdin)
	}
	var stdout, stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	res := &proto.ExecResult{
		Stdout:     stdout.buf.Bytes(),
		Stderr:     stderr.buf.Bytes(),
		Truncated:  stdout.truncated || stderr.truncated,
		DurationNS: time.Since(start).Nanoseconds(),
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return nil, err
	}
	return res, nil
}

// cloned resets everything that siblings restored from one snapshot would
// otherwise share. On a ≥5.18 guest kernel VMGenID already reseeds the RNG on
// restore; mixing in host entropy here is belt and braces, and E3 checks both.
func cloned(c *proto.CloneArgs) error {
	var errs []error
	if len(c.Entropy) > 0 {
		errs = append(errs, addEntropy(c.Entropy))
	}
	if c.UnixNanos > 0 {
		tv := unix.NsecToTimeval(c.UnixNanos)
		errs = append(errs, unix.Settimeofday(&tv))
	}
	if c.Hostname != "" {
		errs = append(errs, unix.Sethostname([]byte(c.Hostname)))
		errs = append(errs, os.WriteFile("/etc/hostname", []byte(c.Hostname+"\n"), 0o644))
	}
	if c.NewMachine {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			errs = append(errs, err)
		} else {
			errs = append(errs, os.WriteFile("/etc/machine-id", []byte(hex.EncodeToString(id[:])+"\n"), 0o444))
		}
	}
	if c.RegenSSH {
		errs = append(errs, regenSSHKeys())
	}
	return errors.Join(errs...)
}

// addEntropy mixes buf into the kernel pool and credits it, via
// RNDADDENTROPY on /dev/random (needs CAP_SYS_ADMIN; the agent runs as root).
func addEntropy(buf []byte) error {
	f, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	info := make([]byte, 8+len(buf))
	binary.NativeEndian.PutUint32(info[0:], uint32(len(buf)*8))
	binary.NativeEndian.PutUint32(info[4:], uint32(len(buf)))
	copy(info[8:], buf)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDADDENTROPY, uintptr(unsafe.Pointer(&info[0]))); e != 0 {
		return fmt.Errorf("RNDADDENTROPY: %w", e)
	}
	return nil
}

func regenSSHKeys() error {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		return nil // template has no sshd; nothing to regenerate
	}
	keys, _ := filepath.Glob("/etc/ssh/ssh_host_*")
	for _, k := range keys {
		_ = os.Remove(k)
	}
	if out, err := exec.Command("ssh-keygen", "-A").CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen -A: %v: %s", err, out)
	}
	return nil
}

func identity() (*proto.Identity, error) {
	id := &proto.Identity{GuestTimeNS: time.Now().UnixNano()}
	id.MachineID = readTrim("/etc/machine-id")
	id.Hostname, _ = os.Hostname()
	id.BootID = readTrim("/proc/sys/kernel/random/boot_id")
	if f := strings.Fields(readTrim("/proc/uptime")); len(f) > 0 {
		id.UptimeSec = f[0]
	}
	if b, err := os.ReadFile("/etc/ssh/ssh_host_ed25519_key.pub"); err == nil {
		sum := sha256.Sum256(b)
		id.SSHHostKey = hex.EncodeToString(sum[:])
	}
	var r [32]byte
	if _, err := rand.Read(r[:]); err != nil {
		return nil, err
	}
	id.URandom = hex.EncodeToString(r[:])
	return id, nil
}

func meminfo() (*proto.MemInfo, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	m := &proto.MemInfo{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			m.TotalKiB = v
		case "MemFree:":
			m.FreeKiB = v
		case "MemAvailable:":
			m.AvailableKiB = v
		case "Cached:":
			m.CachedKiB = v
		}
	}
	return m, nil
}

func readTrim(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}
