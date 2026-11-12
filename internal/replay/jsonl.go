package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

// maxLine is the longest line either parser accepts. Real access-log lines with
// long query strings and user agents run to a few KB; this leaves a wide margin.
const maxLine = 4 << 20

// jsonLine is one line of a JSON Lines trace.
type jsonLine struct {
	At      *float64          `json:"at"`      // seconds from the start of the recording; all lines or none
	Method  string            `json:"method"`  // default: the replay's own default, GET
	Path    string            `json:"path"`    // path and query, starting with "/"
	Headers map[string]string `json:"headers"` // sent in addition to any given on the command line
	Body    string            `json:"body"`
	Status  *int              `json:"status"` // what the server answered when this was recorded; optional
}

// ParseJSONL reads a trace of one JSON object per line:
//
//	{"at": 0.25, "method": "POST", "path": "/orders?id=7", "headers": {"Content-Type": "application/json"}, "body": "{\"qty\":2}"}
//
// Only "path" is required. "status" is what the server answered when the request
// was recorded, so the replay can say how many responses differ. "at" is the time the request was made, in seconds
// from any fixed point: give it to every request or to none. Unknown fields are
// an error, so that a misspelt one ("url", "header") does not silently send the
// wrong thing.
func ParseJSONL(r io.Reader) (*Trace, error) {
	t := &Trace{}
	var stamps []time.Duration
	timedAt := 0 // the line that decided whether the trace is timed
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var l jsonLine
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("line %d: %v", n, err)
		}
		if dec.More() {
			return nil, fmt.Errorf("line %d: more than one JSON value on the line: write one request per line", n)
		}
		req, err := l.request()
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", n, err)
		}

		switch {
		case len(t.Requests) == 0:
			timedAt = n
		case (l.At != nil) != (stamps != nil):
			if stamps != nil {
				return nil, fmt.Errorf(`line %d: has no "at" but line %d does: give every request one, or none`, n, timedAt)
			}
			return nil, fmt.Errorf(`line %d: has an "at" but line %d does not: give every request one, or none`, n, timedAt)
		}
		if l.At != nil {
			if !(*l.At >= 0) || *l.At > 1e9 {
				return nil, fmt.Errorf(`line %d: "at" must be a number of seconds from 0 to 1e9, got %v`, n, *l.At)
			}
			stamps = append(stamps, time.Duration(math.Round(*l.At*1e9)))
		}
		t.Requests = append(t.Requests, req)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the trace: %w", err)
	}
	if len(t.Requests) == 0 {
		return nil, fmt.Errorf("the trace has no requests")
	}
	t.stamps = stamps
	t.sortByTime()
	return t, nil
}

func (l jsonLine) request() (runner.Request, error) {
	if l.Path == "" {
		return runner.Request{}, fmt.Errorf(`"path" is required`)
	}
	if !strings.HasPrefix(l.Path, "/") {
		return runner.Request{}, fmt.Errorf(`"path" must start with "/" (the server comes from the command line), got %q`, l.Path)
	}
	req := runner.Request{Method: strings.ToUpper(l.Method), Target: l.Path}
	if req.Method != "" && !isMethod(req.Method) {
		return runner.Request{}, fmt.Errorf(`"method" must be an HTTP method such as GET, got %q`, l.Method)
	}
	if l.Body != "" {
		req.Body = []byte(l.Body)
	}
	if l.Status != nil {
		if !validStatus(*l.Status) {
			return runner.Request{}, fmt.Errorf(`"status" must be an HTTP status code from 100 to 599, got %d`, *l.Status)
		}
		req.Logged = *l.Status
	}
	if len(l.Headers) > 0 {
		req.Header = http.Header{}
		for k, v := range l.Headers {
			if k == "" || strings.ContainsAny(k, " \t\r\n:") {
				return runner.Request{}, fmt.Errorf("invalid header name %q", k)
			}
			req.Header.Set(k, v)
		}
	}
	return req, nil
}

// isMethod reports whether m looks like an HTTP method: letters only.
func isMethod(m string) bool {
	for _, c := range m {
		if c < 'A' || c > 'Z' { // callers upper-case first
			return false
		}
	}
	return m != ""
}
