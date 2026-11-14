package runner

import (
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
)

// PhaseNames lists the phases a request's corrected latency is split into, in
// the order a request goes through them:
//
//	queue     from the intended send time until Gmeter sent it: waiting for a free worker, or a late timer
//	dns       looking up the host name (only for a request that opened a connection to a name)
//	connect   the TCP connection (only for a request that opened one)
//	tls       the TLS handshake (only for a request that opened an https connection)
//	send      the rest of the time before the server could answer: taking a connection from the pool, writing the request
//	wait      from the request written until the first byte of the response: the server's time and the network's
//	transfer  from the first byte until the whole body was read
//
// They add up to the corrected latency of every request that got a response.
var PhaseNames = [...]string{"queue", "dns", "connect", "tls", "send", "wait", "transfer"}

const numPhases = len(PhaseNames)

// phaseTimes is how long one request spent in each phase.
type phaseTimes struct {
	queue, dns, connect, tls, send, wait, transfer time.Duration
}

func (p phaseTimes) list() [numPhases]time.Duration {
	return [numPhases]time.Duration{p.queue, p.dns, p.connect, p.tls, p.send, p.wait, p.transfer}
}

func (p phaseTimes) sum() time.Duration {
	var total time.Duration
	for _, d := range p.list() {
		total += d
	}
	return total
}

// phaseClock records when the events of one request happened, from the hooks of
// net/http/httptrace, and splits the request into phases afterwards.
//
// The hooks run on goroutines of the HTTP transport, and the transport may run
// one after the request has returned (a dial it started that lost a race to an
// idle connection), so every field is guarded by the mutex.
type phaseClock struct {
	now func() time.Time

	mu                        sync.Mutex
	dnsStart, dnsDone         time.Time
	connectStart, connectDone time.Time
	tlsStart, tlsDone         time.Time
	wrote, first              time.Time
	reused                    bool // the request used a connection that already existed
}

func newPhaseClock(now func() time.Time) *phaseClock { return &phaseClock{now: now} }

// stamp sets *t to the current time. With overwrite false it keeps the first
// time that was set.
func (c *phaseClock) stamp(t *time.Time, overwrite bool) {
	now := c.now()
	c.mu.Lock()
	if overwrite || t.IsZero() {
		*t = now
	}
	c.mu.Unlock()
}

// trace returns the hooks that feed this clock.
func (c *phaseClock) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { c.stamp(&c.dnsStart, false) },
		DNSDone:  func(httptrace.DNSDoneInfo) { c.stamp(&c.dnsDone, true) },
		// A name with several addresses can be dialled in several attempts: the
		// connect phase runs from the first attempt to the one that succeeded.
		ConnectStart: func(string, string) { c.stamp(&c.connectStart, false) },
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				c.stamp(&c.connectDone, true)
			}
		},
		TLSHandshakeStart: func() { c.stamp(&c.tlsStart, false) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				c.stamp(&c.tlsDone, true)
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			c.mu.Lock()
			c.reused = info.Reused
			c.mu.Unlock()
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { c.stamp(&c.wrote, true) },
		GotFirstResponseByte: func() { c.stamp(&c.first, false) },
	}
}

// split divides a request that got a response into its phases. intended is when
// it was due, sent when Gmeter handed it to the HTTP client, and finished when
// its body had been read.
//
// The connection phases count only for a request that used a connection it
// opened itself: one that took an idle connection was not delayed by a dial
// that happened to be under way. Send is what is left, so the phases add up to
// finished minus intended exactly.
func (c *phaseClock) split(intended, sent, finished time.Time) phaseTimes {
	c.mu.Lock()
	defer c.mu.Unlock()

	p := phaseTimes{queue: sent.Sub(intended)}
	if !c.reused {
		// On HTTP/2 a request can join a dial another request began before this
		// one was sent: only what it waited for, from when it was sent, is its own.
		from := func(t time.Time) time.Time {
			if t.IsZero() || t.After(sent) {
				return t
			}
			return sent
		}
		p.dns = span(from(c.dnsStart), c.dnsDone)
		p.connect = span(from(c.connectStart), c.connectDone)
		p.tls = span(from(c.tlsStart), c.tlsDone)
	}
	if !c.first.IsZero() {
		p.transfer = max(0, finished.Sub(c.first))
		if !c.wrote.IsZero() { // a server can answer before it has read the whole request
			p.wait = max(0, c.first.Sub(c.wrote))
		}
	}
	p.send = finished.Sub(sent) - p.dns - p.connect - p.tls - p.wait - p.transfer
	// Hooks reported out of order (a dial that finished after the response
	// began) can make the phases overlap. The connection phases give way, the
	// last first, so that none is negative and they still add up exactly.
	for _, d := range []*time.Duration{&p.tls, &p.connect, &p.dns} {
		if p.send >= 0 {
			break
		}
		cut := min(*d, -p.send)
		*d -= cut
		p.send += cut
	}
	return p
}

// span is how long from start to end, or zero if either did not happen.
// span is how long a phase took, or 0 if it did not happen. A phase that
// happened never reads zero: where the clock advances in steps (Windows, by a
// millisecond or more) a short dial starts and ends at the same reading, and
// 0 would make it look like a request that never connected.
func span(start, end time.Time) time.Duration {
	if start.IsZero() || end.IsZero() {
		return 0
	}
	return max(time.Nanosecond, end.Sub(start))
}

// PhaseResult is the time every request that got a response spent in one phase.
type PhaseResult struct {
	Phase string
	// Samples has one sample per request that had the phase: queue, send, wait
	// and transfer every request, dns, connect and tls only those that opened
	// a connection.
	Samples *hist.Histogram
}

// phaseTally collects the phases of all requests. Every worker records into it,
// so it has a lock, which is cheap next to an HTTP round trip.
type phaseTally struct {
	mu sync.Mutex
	h  [numPhases]*hist.Histogram
}

func newPhaseTally() *phaseTally {
	t := &phaseTally{}
	for i := range t.h {
		t.h[i] = hist.New()
	}
	return t
}

// record adds one request. A connection phase that did not happen (a zero) is
// not a sample: the average connect time is of the requests that connected.
func (t *phaseTally) record(p phaseTimes) {
	t.mu.Lock()
	for i, d := range p.list() {
		if d > 0 || !optionalPhase(i) {
			t.h[i].Record(d)
		}
	}
	t.mu.Unlock()
}

// PhaseTotal is the time a set of requests (one label's) spent in one phase,
// added up. Unlike PhaseResult it keeps no distribution, only the total, which
// is all that is needed to say where a label's time went and costs a few bytes
// instead of a quarter of a megabyte.
type PhaseTotal struct {
	Phase string
	// Requests is how many requests had the phase, as for PhaseResult.Samples.
	Requests uint64
	// Total is the time they spent in it. Over all phases the totals add up to
	// the label's corrected latencies, added up.
	Total time.Duration
}

// phaseSums accumulates PhaseTotals, a phase at a time; it has no lock of its
// own.
type phaseSums struct {
	requests [numPhases]uint64
	total    [numPhases]time.Duration

	byName phaseTotalSums // for merging finished totals, by addTotals
}

// add records one request, counting a connection phase only if it happened, as
// phaseTally.record does.
func (s *phaseSums) add(p phaseTimes) {
	for i, d := range p.list() {
		if d > 0 || !optionalPhase(i) {
			s.requests[i]++
			s.total[i] += d
		}
	}
}

func (s *phaseSums) totals() []PhaseTotal {
	out := make([]PhaseTotal, numPhases)
	for i, name := range PhaseNames {
		out[i] = PhaseTotal{Phase: name, Requests: s.requests[i], Total: s.total[i]}
	}
	return out
}

// phaseTotalSums adds up PhaseTotals by phase name, for merging.
type phaseTotalSums = map[string]PhaseTotal

// addTotals adds totals, a phase at a time, keyed by the phase's name.
func (s *phaseSums) addTotals(totals []PhaseTotal) {
	if s.byName == nil {
		s.byName = phaseTotalSums{}
	}
	for _, t := range totals {
		x := s.byName[t.Phase]
		x.Phase = t.Phase
		x.Requests += t.Requests
		x.Total += t.Total
		s.byName[t.Phase] = x
	}
}

// totalsOf returns the added-up totals in the order of the phases of like.
func (s *phaseSums) totalsOf(like []PhaseTotal) []PhaseTotal {
	out := make([]PhaseTotal, len(like))
	for i, t := range like {
		out[i] = s.byName[t.Phase]
		out[i].Phase = t.Phase
	}
	return out
}

// optionalPhase reports whether phase i happens only to some requests.
func optionalPhase(i int) bool { return i >= 1 && i <= 3 } // dns, connect, tls

// results returns the phases in order. The workers must have exited.
func (t *phaseTally) results() []PhaseResult {
	out := make([]PhaseResult, numPhases)
	for i, name := range PhaseNames {
		out[i] = PhaseResult{Phase: name, Samples: t.h[i]}
	}
	return out
}
