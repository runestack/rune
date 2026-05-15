package portforwarddaemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// Request types accepted by the daemon over its unix socket.
const (
	CmdPing    = "ping"
	CmdAdd     = "add"
	CmdList    = "list"
	CmdStop    = "stop"
	CmdStopAll = "stop_all"
	CmdLogs    = "logs"
)

// Request is one CLI → daemon RPC. Exactly one of the typed fields is
// populated based on Cmd.
type Request struct {
	Cmd     string   `json:"cmd"`
	Forward *Forward `json:"forward,omitempty"` // for Add
	ID      string   `json:"id,omitempty"`      // for Stop, Logs
	Tail    int      `json:"tail,omitempty"`    // for Logs
}

// Response is one daemon → CLI reply. OK is the only field guaranteed
// to be set; the rest are command-specific.
type Response struct {
	OK       bool       `json:"ok"`
	Error    string     `json:"error,omitempty"`
	Version  string     `json:"version,omitempty"`  // Ping
	Forward  *Forward   `json:"forward,omitempty"`  // Add
	Forwards []*Forward `json:"forwards,omitempty"` // List
	Stopped  int        `json:"stopped,omitempty"`  // StopAll
	Lines    []string   `json:"lines,omitempty"`    // Logs
}

// WriteJSONLine writes one length-prefixed JSON object to w. We use a
// trailing newline as the framing terminator — every Request /
// Response fits comfortably in well under a megabyte and pretty
// printers / debugging is trivial.
func WriteJSONLine(w io.Writer, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// ReadJSONLine reads one newline-terminated JSON object from r into v.
func ReadJSONLine(r *bufio.Reader, v interface{}) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	if len(line) == 0 {
		return fmt.Errorf("empty line")
	}
	return json.Unmarshal(line, v)
}

// DialDaemon opens a connection to the daemon socket. Returns a net.Conn
// the caller must Close.
func DialDaemon(socketPath string) (net.Conn, error) {
	return net.Dial("unix", socketPath)
}
