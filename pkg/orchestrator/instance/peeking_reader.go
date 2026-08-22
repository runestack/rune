// PeekingReader, an io.ReadCloser wrapper that can
// answer "is this stream non-empty?" without consuming it. Used by the
// attach path here and by the service controller's merged log reader.

package instance

import "io"

// PeekingReader wraps an io.ReadCloser so we can answer "is this
// stream actually going to produce anything?" without consuming the
// data. The first byte is buffered on Peek/HasData and re-emitted
// on the next Read, so the wrapper is transparent to downstream
// consumers (MultiLogStreamer, the CLI client). Used by
// GetServiceLogs to detect silent live containers and fall back to
// the previous tombstone's LastLogs.
type PeekingReader struct {
	rc       io.ReadCloser
	peek     []byte // buffered first byte; len() > 0 means "stream had data"
	peeked   bool   // first read has been attempted
	peekDone bool   // returned EOF/error during peek (no more data)
}

func NewPeekingReader(rc io.ReadCloser) *PeekingReader {
	return &PeekingReader{rc: rc}
}

// HasData returns true if the underlying stream produced at least
// one byte. Triggers the lazy first read on first call. Safe to call
// multiple times; cached. On a read error the byte (if any) is still
// surfaced — we don't want to treat a partial first-byte-then-error
// as "no data" and silently mask the real failure.
func (p *PeekingReader) HasData() (bool, error) {
	if p.peeked {
		return len(p.peek) > 0, nil
	}
	p.peeked = true
	buf := make([]byte, 1)
	n, err := p.rc.Read(buf)
	if n > 0 {
		p.peek = buf[:n]
	}
	if err == io.EOF {
		p.peekDone = true
		return n > 0, nil
	}
	return n > 0, err
}

func (p *PeekingReader) Read(buf []byte) (int, error) {
	if len(p.peek) > 0 {
		n := copy(buf, p.peek)
		p.peek = p.peek[n:]
		return n, nil
	}
	if p.peekDone {
		return 0, io.EOF
	}
	return p.rc.Read(buf)
}

func (p *PeekingReader) Close() error { return p.rc.Close() }
