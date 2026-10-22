package main

import (
	"bufio"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/runestack/rune/spike/microvm/internal/proto"
)

func TestHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	cases := []struct {
		name  string
		req   proto.Request
		check func(*proto.Response) bool
	}{
		{"ping", proto.Request{Op: proto.OpPing}, func(r *proto.Response) bool { return r.OK }},
		{"exec ok", proto.Request{Op: proto.OpExec, Exec: &proto.ExecArgs{Argv: []string{"sh", "-c", "echo hi; echo err >&2"}}},
			func(r *proto.Response) bool {
				return r.OK && string(r.Exec.Stdout) == "hi\n" && string(r.Exec.Stderr) == "err\n" && r.Exec.ExitCode == 0
			}},
		{"exec exit code", proto.Request{Op: proto.OpExec, Exec: &proto.ExecArgs{Argv: []string{"sh", "-c", "exit 7"}}},
			func(r *proto.Response) bool { return r.OK && r.Exec.ExitCode == 7 }},
		{"exec stdin", proto.Request{Op: proto.OpExec, Exec: &proto.ExecArgs{Argv: []string{"cat"}, Stdin: []byte("in")}},
			func(r *proto.Response) bool { return r.OK && string(r.Exec.Stdout) == "in" }},
		{"exec timeout", proto.Request{Op: proto.OpExec, Exec: &proto.ExecArgs{Argv: []string{"sleep", "5"}, TimeoutMS: 50}},
			func(r *proto.Response) bool { return r.OK && r.Exec.ExitCode != 0 }},
		{"exec missing binary", proto.Request{Op: proto.OpExec, Exec: &proto.ExecArgs{Argv: []string{"/nonexistent"}}},
			func(r *proto.Response) bool { return !r.OK && r.Error != "" }},
		{"write", proto.Request{Op: proto.OpWriteFile, File: &proto.FileArgs{Path: path, Data: []byte("data")}},
			func(r *proto.Response) bool { return r.OK }},
		{"read", proto.Request{Op: proto.OpReadFile, File: &proto.FileArgs{Path: path}},
			func(r *proto.Response) bool { return r.OK && string(r.Data) == "data" }},
		{"meminfo", proto.Request{Op: proto.OpMemInfo},
			func(r *proto.Response) bool { return r.OK && r.Mem.TotalKiB > 0 && r.Mem.AvailableKiB > 0 }},
		{"identity", proto.Request{Op: proto.OpIdentity},
			func(r *proto.Response) bool { return r.OK && len(r.Identity.URandom) == 64 && r.Identity.BootID != "" }},
		{"unknown", proto.Request{Op: "nope"}, func(r *proto.Response) bool { return !r.OK }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := handle(&c.req)
			if !c.check(r) {
				t.Errorf("unexpected response: %+v exec=%+v", r, r.Exec)
			}
		})
	}
}

func TestOutputIsCapped(t *testing.T) {
	r := handle(&proto.Request{Op: proto.OpExec, Exec: &proto.ExecArgs{Argv: []string{"head", "-c", "2000000", "/dev/zero"}}})
	if !r.OK || len(r.Exec.Stdout) != maxOutput || !r.Exec.Truncated {
		t.Fatalf("ok=%v len=%d truncated=%v", r.OK, len(r.Exec.Stdout), r.Exec.Truncated)
	}
}

func TestAddEntropy(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("RNDADDENTROPY needs root")
	}
	if err := addEntropy(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
}

// TestServeOverSocket runs the real framing loop over a socketpair.
func TestServeOverSocket(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	go serve(os.NewFile(uintptr(fds[1]), "agent"))
	host := os.NewFile(uintptr(fds[0]), "host")
	defer host.Close()
	r := bufio.NewReader(host)
	for id := uint64(1); id <= 3; id++ {
		if err := proto.Write(host, &proto.Request{ID: id, Op: proto.OpPing}); err != nil {
			t.Fatal(err)
		}
		var resp proto.Response
		if err := proto.Read(r, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.ID != id || !resp.OK {
			t.Fatalf("response %+v for id %d", resp, id)
		}
	}
}
