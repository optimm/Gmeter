package runner

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func TestPreparingARunDoesNotCopyTheRequests(t *testing.T) {
	const n = 100000
	reqs := make([]Request, n)
	for i := range reqs {
		reqs[i] = Request{Target: fmt.Sprintf("/products/%d?ref=home", i), Label: "GET /products/:id"}
	}
	cfg := Config{URL: "http://127.0.0.1:1/api", Rate: 10, Duration: time.Second, Workers: 1, Requests: reqs}

	before := liveHeap()
	prepared, err := cfg.normalize()
	if err != nil {
		t.Fatal(err)
	}
	perRequest := float64(int64(liveHeap())-int64(before)) / n
	runtime.KeepAlive(prepared)
	runtime.KeepAlive(reqs)
	// A copy of the requests is about 100 bytes each, and their full URLs 40
	// more; which label each request has needs one byte.
	if perRequest > 4 {
		t.Errorf("preparing the run kept %.1f bytes a request alive, want at most 4: it copied the requests", perRequest)
	}
}
