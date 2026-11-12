package replay

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

// clfLine picks the timestamp, the quoted request and the status out of a
// common or combined log format line:
//
//	host ident user [10/Oct/2023:13:55:36 -0700] "GET /path HTTP/1.1" 200 2326 ...
//
// The status is optional: a line without a usable one is still a request.
var clfLine = regexp.MustCompile(`^\S+ \S+ \S+ \[([^\]]+)\] "([^"]*)"(?: (\S+))?`)

const clfTime = "02/Jan/2006:15:04:05 -0700"

// ParseCLF reads an access log in the common or the combined log format, which
// nginx, Apache, HAProxy and most proxies can write. Each line gives a method,
// a path and the second the request was logged in. A log has no headers and no
// request bodies, so replayed POSTs carry no body. Lines that are not usable
// requests are counted in Trace.Unparsed.
//
// Servers log a request when it finishes, so the times are completion times,
// and lines can be slightly out of order; the trace is sorted by time.
func ParseCLF(r io.Reader) (*Trace, error) {
	t := &Trace{}
	var stamps []time.Duration
	lines := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lines++
		req, stamp, ok := parseCLFLine(line)
		if !ok {
			t.Unparsed++
			continue
		}
		t.Requests = append(t.Requests, req)
		stamps = append(stamps, stamp)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the log: %w", err)
	}
	switch {
	case lines == 0:
		return nil, fmt.Errorf("the log has no requests")
	case len(t.Requests) == 0:
		return nil, fmt.Errorf("none of the %d lines is in the common log format (host ident user [time] \"METHOD /path PROTO\" ...)", lines)
	}
	t.stamps, t.resolution = stamps, time.Second
	t.sortByTime()
	return t, nil
}

func parseCLFLine(line string) (runner.Request, time.Duration, bool) {
	m := clfLine.FindStringSubmatch(line)
	if m == nil {
		return runner.Request{}, 0, false
	}
	when, err := time.Parse(clfTime, m[1])
	if err != nil {
		return runner.Request{}, 0, false
	}
	f := strings.Fields(m[2]) // METHOD target [PROTOCOL]; "-" for a connection that sent nothing
	if len(f) < 2 || !isMethod(strings.ToUpper(f[0])) || !strings.HasPrefix(f[1], "/") {
		return runner.Request{}, 0, false
	}
	// Copies, not substrings: a substring would keep the whole line in memory
	// for as long as the request, and a line can be kilobytes of user agent.
	req := runner.Request{Method: method(f[0]), Target: strings.Clone(f[1])}
	if n, err := strconv.Atoi(m[3]); err == nil && validStatus(n) { // "-" and garbage leave it 0
		req.Logged = n
	}
	return req, time.Duration(when.Unix()) * time.Second, true
}

// validStatus reports whether n is an HTTP status code.
func validStatus(n int) bool { return n >= 100 && n <= 599 }

// commonMethods are shared by every request that uses one, rather than each
// keeping its own copy.
var commonMethods = map[string]string{}

func init() {
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE"} {
		commonMethods[m] = m
	}
}

// method is m in upper case, in a string of its own.
func method(m string) string {
	m = strings.ToUpper(m)
	if shared, ok := commonMethods[m]; ok {
		return shared
	}
	return strings.Clone(m)
}
