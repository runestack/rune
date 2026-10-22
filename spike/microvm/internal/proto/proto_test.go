package proto

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Request{ID: 9, Op: OpExec, Exec: &ExecArgs{Argv: []string{"echo", "hi"}, Stdin: []byte{0, 1, 2}}}
	if err := Write(&buf, &in); err != nil {
		t.Fatal(err)
	}
	var out Request
	if err := Read(&buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != 9 || out.Op != OpExec || out.Exec.Argv[1] != "hi" || !bytes.Equal(out.Exec.Stdin, []byte{0, 1, 2}) {
		t.Fatalf("got %+v", out)
	}
}

func TestRejectsOversizedFrame(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrame+1)
	var out Request
	if err := Read(bytes.NewReader(hdr[:]), &out); err == nil {
		t.Fatal("expected an error for an oversized frame")
	}
}
