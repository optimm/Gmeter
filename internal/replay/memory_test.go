package replay

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

// liveHeap is the memory in use after a full collection.
func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// totalAllocated is the memory allocated so far, freed or not.
func totalAllocated() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

const memoryRequests = 20000

// longLineLog is an access log whose lines carry a 2KB user agent, as real
// combined-format lines with long cookies or agents can.
func longLineLog(n int, method string) string {
	agent := strings.Repeat("x", 2048)
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "10.0.0.1 - - [12/Nov/2026:13:00:%02d +0000] \"%s /products/%d?ref=home HTTP/1.1\" 200 512 \"-\" \"%s\"\n", i%60/10*10, method, i, agent)
	}
	return b.String()
}

func TestAParsedRequestDoesNotKeepItsLogLineInMemory(t *testing.T) {
	for _, method := range []string{"GET", "PROPFIND"} { // a common method and a rare one
		log := longLineLog(memoryRequests, method)
		before := liveHeap()
		tr, err := ParseCLF(strings.NewReader(log))
		if err != nil {
			t.Fatal(err)
		}
		perRequest := float64(liveHeap()-before) / memoryRequests
		runtime.KeepAlive(tr)
		runtime.KeepAlive(log) // only what parsing added counts
		// A request is a struct of about 100 bytes, its path and a time. Keeping a
		// substring of the line would keep all of the line's 2KB.
		if perRequest > 400 {
			t.Errorf("%s: a parsed request keeps %.0f bytes alive, want under 400: it is holding on to its log line", method, perRequest)
		}
		if tr.Requests[0].Method != method {
			t.Errorf("method = %q, want %q", tr.Requests[0].Method, method)
		}
	}
}

func TestParsedJSONLinesDoNotKeepTheirLinesInMemory(t *testing.T) {
	pad := strings.Repeat("y", 2048)
	var b strings.Builder
	for i := range memoryRequests {
		fmt.Fprintf(&b, "{\"at\": %d, \"method\": \"get\", \"path\": \"/products/%d\", \"body\": \"\", \"headers\": {\"X-Pad\": \"%s\"}}\n", i, i, pad[:i%2])
	}
	// The padding is in the line, not in what is kept: lines are long, headers short.
	log := strings.ReplaceAll(b.String(), `"body": ""`, `"body": ""`+strings.Repeat(" ", 2048))
	before := liveHeap()
	tr, err := ParseJSONL(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	perRequest := float64(liveHeap()-before) / memoryRequests
	runtime.KeepAlive(tr)
	runtime.KeepAlive(log) // only what parsing added counts
	if perRequest > 600 {
		t.Errorf("a parsed request keeps %.0f bytes alive, want under 600: it is holding on to its line", perRequest)
	}
}

func TestEndpointLabelsAreSharedNotCopiedPerRequest(t *testing.T) {
	tr := &Trace{Requests: make([]runner.Request, memoryRequests)}
	for i := range tr.Requests {
		tr.Requests[i] = runner.Request{Method: "GET", Target: fmt.Sprintf("/products/%d", i)}
	}
	before := liveHeap()
	if err := tr.LabelEndpoints(20); err != nil {
		t.Fatal(err)
	}
	grew := int64(liveHeap()) - int64(before)
	runtime.KeepAlive(tr)
	if perRequest := float64(grew) / memoryRequests; perRequest > 4 {
		t.Errorf("labelling kept %.1f bytes a request alive, want under 4: every request of an endpoint can share one label", perRequest)
	}
	if tr.Requests[0].Label != "GET /products/:id" {
		t.Errorf("label = %q", tr.Requests[0].Label)
	}
}

func TestSortingByTimeDoesNotCopyTheRequests(t *testing.T) {
	for _, name := range []string{"in order", "out of order"} {
		tr := &Trace{Requests: make([]runner.Request, memoryRequests), stamps: make([]time.Duration, memoryRequests)}
		for i := range tr.Requests {
			tr.Requests[i].Target = fmt.Sprintf("/%d", i)
			tr.stamps[i] = time.Duration(i) * time.Second
		}
		if name == "out of order" {
			for i := 0; i+1 < memoryRequests; i += 2 { // swap neighbours
				tr.Requests[i], tr.Requests[i+1] = tr.Requests[i+1], tr.Requests[i]
				tr.stamps[i], tr.stamps[i+1] = tr.stamps[i+1], tr.stamps[i]
			}
		}
		before := totalAllocated()
		tr.sortByTime()
		perRequest := float64(totalAllocated()-before) / memoryRequests
		// A copy of the requests is about 100 bytes each; an index to sort by is 8,
		// and a trace already in order needs neither.
		limit := 16.0
		if name == "in order" {
			limit = 0.5
		}
		if perRequest > limit {
			t.Errorf("%s: sorting allocated %.1f bytes a request, want at most %g", name, perRequest, limit)
		}
		for i, r := range tr.Requests {
			if want := fmt.Sprintf("/%d", i); r.Target != want || tr.stamps[i] != time.Duration(i)*time.Second {
				t.Fatalf("%s: request %d is %s at %v, want %s at %ds", name, i, r.Target, tr.stamps[i], want, i)
			}
		}
	}
}

func TestSortingByTimeInPlaceHandlesAnyOrder(t *testing.T) {
	const n = 1000
	tr := &Trace{Requests: make([]runner.Request, n), stamps: make([]time.Duration, n)}
	perm := rand.New(rand.NewPCG(1, 2)).Perm(n)
	for i, p := range perm {
		// Every request at an even second has a twin at the same time: ties stay in file order.
		tr.Requests[i].Target = fmt.Sprintf("/%d", p)
		tr.stamps[i] = time.Duration(p/2) * time.Second
	}
	tr.sortByTime()
	seen := map[time.Duration]string{}
	for i, r := range tr.Requests {
		if i > 0 && tr.stamps[i] < tr.stamps[i-1] {
			t.Fatalf("request %d at %v comes after one at %v", i, tr.stamps[i], tr.stamps[i-1])
		}
		if want := time.Duration(i/2) * time.Second; tr.stamps[i] != want {
			t.Fatalf("request %d is at %v, want %v", i, tr.stamps[i], want)
		}
		var p int
		fmt.Sscanf(r.Target, "/%d", &p)
		if p/2 != i/2 {
			t.Fatalf("request %d is %s, which was at %ds, but sits at %v", i, r.Target, p/2, tr.stamps[i])
		}
		if prev, ok := seen[tr.stamps[i]]; ok {
			if firstIndex(perm, prev) > firstIndex(perm, r.Target) {
				t.Errorf("tie at %v: %s came before %s in the file but sorts after it", tr.stamps[i], r.Target, prev)
			}
		}
		seen[tr.stamps[i]] = r.Target
	}
}

// firstIndex is the position in the file of the request with the given target.
func firstIndex(perm []int, target string) int {
	var p int
	fmt.Sscanf(target, "/%d", &p)
	for i, q := range perm {
		if q == p {
			return i
		}
	}
	return -1
}

func TestKeepingMethodsFiltersInPlace(t *testing.T) {
	tr := &Trace{Requests: make([]runner.Request, memoryRequests), stamps: make([]time.Duration, memoryRequests)}
	for i := range tr.Requests {
		tr.Requests[i] = runner.Request{Method: "GET", Target: fmt.Sprintf("/%d", i)}
		if i%2 == 1 {
			tr.Requests[i].Method = "POST"
		}
		tr.stamps[i] = time.Duration(i)
	}
	before := totalAllocated()
	dropped := tr.KeepMethods("GET")
	if perRequest := float64(totalAllocated()-before) / memoryRequests; perRequest > 0.5 {
		t.Errorf("keeping the GETs allocated %.1f bytes a request, want none: it copied the requests", perRequest)
	}
	if dropped != memoryRequests/2 || len(tr.Requests) != memoryRequests/2 || len(tr.stamps) != memoryRequests/2 {
		t.Fatalf("dropped %d, kept %d requests and %d times", dropped, len(tr.Requests), len(tr.stamps))
	}
	for i, r := range tr.Requests {
		if want := fmt.Sprintf("/%d", 2*i); r.Target != want || tr.stamps[i] != time.Duration(2*i) {
			t.Fatalf("request %d is %s at %v, want %s at %v", i, r.Target, tr.stamps[i], want, time.Duration(2*i))
		}
	}
}

func TestDroppedRequestsAreFreed(t *testing.T) {
	const n = 1000
	tr := &Trace{Requests: make([]runner.Request, n)}
	for i := range tr.Requests {
		tr.Requests[i] = runner.Request{Method: "GET", Target: "/"}
		if i%2 == 1 {
			tr.Requests[i] = runner.Request{Method: "POST", Target: "/", Body: make([]byte, 10<<10)}
		}
	}
	before := liveHeap()
	tr.KeepMethods("GET")
	freed := int64(before) - int64(liveHeap())
	runtime.KeepAlive(tr)
	if want := int64(n / 2 * 10 << 10 * 9 / 10); freed < want {
		t.Errorf("dropping 500 POSTs with 10KB bodies freed %d bytes, want at least %d", freed, want)
	}
}
