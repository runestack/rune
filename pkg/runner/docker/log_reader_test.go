package docker

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

const (
	streamStdout = 1
	streamStderr = 2
)

// frame builds one Docker multiplexed log frame: 8-byte header
// [stream][0][0][0][size uint32 BE] followed by the payload.
func frame(stream byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, []byte(payload)...)
}

func mux(frames ...[]byte) io.ReadCloser {
	var b bytes.Buffer
	for _, f := range frames {
		b.Write(f)
	}
	return io.NopCloser(&b)
}

// peekOneByte mirrors controllers.peekingReader.HasData: a 1-byte Read the
// serve path performs before handing the stream to a consumer. It exercises
// the reader mid-frame; the byte is prepended back so no data is lost.
func peekOneByte(t *testing.T, rc io.ReadCloser) io.ReadCloser {
	t.Helper()
	b := make([]byte, 1)
	n, err := rc.Read(b)
	if err != nil && err != io.EOF {
		t.Fatalf("peek read: %v", err)
	}
	if n == 0 {
		return rc
	}
	return &prepend{first: b[:n], rc: rc}
}

type prepend struct {
	first []byte
	rc    io.ReadCloser
}

func (p *prepend) Read(b []byte) (int, error) {
	if len(p.first) > 0 {
		n := copy(b, p.first)
		p.first = p.first[n:]
		return n, nil
	}
	return p.rc.Read(b)
}
func (p *prepend) Close() error { return p.rc.Close() }

// TestDockerLogReaderNeverReturnsZeroNil is the core #160 assertion: with a
// non-empty buffer, Read must never return (0, nil) — it makes progress or
// reports io.EOF. Zero-length frames must not surface as no-progress reads.
func TestDockerLogReaderNeverReturnsZeroNil(t *testing.T) {
	stream := mux(
		frame(streamStdout, ""), // three consecutive zero-frames …
		frame(streamStderr, ""),
		frame(streamStdout, ""),
		frame(streamStdout, "payload\n"), // … then real data
	)
	r := newLogReader(stream)
	buf := make([]byte, 4096)
	for i := 0; i < 1000; i++ {
		n, err := r.Read(buf)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n == 0 {
			t.Fatal("Read returned (0, nil) — violates the contract bufio.Scanner relies on")
		}
	}
	t.Fatal("reader never reached EOF")
}

// TestDockerLogReaderManyZeroFramesScanner is the strongest regression: a run
// of zero-length frames longer than bufio.Scanner's maxConsecutiveEmptyReads
// (100) BEFORE any real line. The old reader returned (0, nil) for each,
// tripping io.ErrNoProgress and dropping the whole stream; the fixed reader
// skips them and the Scanner sees every line.
func TestDockerLogReaderManyZeroFramesScanner(t *testing.T) {
	frames := make([][]byte, 0, 260)
	for i := 0; i < 150; i++ {
		frames = append(frames, frame(streamStdout, "")) // 150 empties up front
	}
	frames = append(frames,
		frame(streamStdout, "line-1\n"),
		frame(streamStderr, ""), // interleaved empties between lines
		frame(streamStdout, "line-2\n"),
		frame(streamStdout, "line-3\n"),
		frame(streamStdout, ""), // trailing empty
	)

	r := peekOneByte(t, newLogReader(mux(frames...)))
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error (the #160 bug: zero-frames stalled the reader): %v", err)
	}
	if want := "line-1|line-2|line-3"; strings.Join(got, "|") != want {
		t.Fatalf("lost lines to zero-frame stall: got %q want %q", strings.Join(got, "|"), want)
	}
}

// TestDockerLogReaderDemuxesNormally guards the happy path: well-formed frames
// still demux to the exact concatenated payload.
func TestDockerLogReaderDemuxesNormally(t *testing.T) {
	r := newLogReader(mux(
		frame(streamStdout, "hello\n"),
		frame(streamStderr, "err-line\n"),
		frame(streamStdout, "world\n"),
	))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if want := "hello\nerr-line\nworld\n"; string(out) != want {
		t.Fatalf("demux mismatch: got %q want %q", string(out), want)
	}
}
