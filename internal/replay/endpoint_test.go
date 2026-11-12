package replay

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

func TestEndpointNamesTheMethodAndPathWithIdentifiersCollapsed(t *testing.T) {
	for _, tc := range []struct{ method, target, want string }{
		{"GET", "/orders", "GET /orders"},
		{"", "/orders", "GET /orders"}, // no method is sent as GET
		{"post", "/orders", "POST /orders"},
		{"GET", "/", "GET /"},
		{"GET", "/orders/", "GET /orders/"}, // a trailing slash is part of the path
		{"GET", "/a//b", "GET /a//b"},

		// The query string is dropped: it is where tokens and search terms live.
		{"GET", "/orders?id=7&token=secret", "GET /orders"},
		{"GET", "/search?q=a/b/12345", "GET /search"},

		// Identifiers become :id, so a million different orders are one endpoint.
		{"GET", "/orders/12345", "GET /orders/:id"},
		{"GET", "/orders/12345/items/9", "GET /orders/:id/items/:id"},
		{"GET", "/orders/99999999999999999999", "GET /orders/:id"},
		{"GET", "/users/123e4567-e89b-12d3-a456-426614174000", "GET /users/:id"},
		{"GET", "/users/123E4567-E89B-12D3-A456-426614174000", "GET /users/:id"},
		{"GET", "/blobs/0123456789abcdef", "GET /blobs/:id"},
		{"GET", "/blobs/0123456789ABCDEF0123456789abcdef", "GET /blobs/:id"},

		// Anything else is a name, not an identifier.
		{"GET", "/v2/orders", "GET /v2/orders"},
		{"GET", "/colors/abc123", "GET /colors/abc123"},
		{"GET", "/colors/deadbeef", "GET /colors/deadbeef"}, // hex, but too short to be a hash
		{"GET", "/blobs/0123456789abcde", "GET /blobs/0123456789abcde"},
		{"GET", "/img/123.png", "GET /img/123.png"},
		{"GET", "/orders/12a", "GET /orders/12a"},
		{"GET", "/users/123e4567-e89b-12d3-a456-42661417400", "GET /users/123e4567-e89b-12d3-a456-42661417400"}, // a digit short
	} {
		if got := Endpoint(tc.method, tc.target); got != tc.want {
			t.Errorf("Endpoint(%q, %q) = %q, want %q", tc.method, tc.target, got, tc.want)
		}
	}
}

// traceOf returns a trace of GET requests, count[i] of them to target i.
func traceOf(targets []string, count []int) *Trace {
	t := &Trace{}
	for i, n := range count {
		for range n {
			t.Requests = append(t.Requests, runner.Request{Target: targets[i]})
		}
	}
	return t
}

func labelCounts(t *Trace) map[string]int {
	got := map[string]int{}
	for _, r := range t.Requests {
		got[r.Label]++
	}
	return got
}

func TestLabelEndpointsGivesEveryRequestItsEndpoint(t *testing.T) {
	tr := &Trace{Requests: []runner.Request{
		{Target: "/orders/1"}, {Method: "POST", Target: "/orders"}, {Target: "/orders/2?x=1"}, {Target: "/health"},
	}}

	if err := tr.LabelEndpoints(10); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"GET /orders/:id": 2, "POST /orders": 1, "GET /health": 1}
	if got := labelCounts(tr); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if got := tr.Requests[2].Target; got != "/orders/2?x=1" {
		t.Errorf("target became %q: labelling must not change what is sent", got)
	}
}

func TestLabelEndpointsKeepsTheBusiestAndGroupsTheRest(t *testing.T) {
	targets := []string{"/a", "/b", "/c", "/d", "/e"}
	tr := traceOf(targets, []int{5, 3, 2, 2, 1})

	if err := tr.LabelEndpoints(3); err != nil {
		t.Fatal(err)
	}
	// At most 3 rows: the two busiest and everything else together.
	want := map[string]int{"GET /a": 5, "GET /b": 3, OtherEndpoints: 5}
	if got := labelCounts(tr); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

func TestLabelEndpointsDoesNotGroupWhenThereIsRoom(t *testing.T) {
	tr := traceOf([]string{"/a", "/b", "/c"}, []int{3, 2, 1})

	if err := tr.LabelEndpoints(3); err != nil {
		t.Fatal(err)
	}
	if got := labelCounts(tr); got[OtherEndpoints] != 0 || len(got) != 3 {
		t.Errorf("labels = %v, want three endpoints and no %s", got, OtherEndpoints)
	}
}

func TestLabelEndpointsBreaksTiesByNameNotByOrderInTheTrace(t *testing.T) {
	// /b, /c and /d are equally busy and only one fits next to /a, whatever
	// order they come in.
	for _, targets := range [][]string{{"/a", "/b", "/c", "/d"}, {"/d", "/c", "/b", "/a"}} {
		counts := []int{2, 2, 2, 2}
		if targets[0] == "/a" {
			counts[0] = 5
		} else {
			counts[3] = 5
		}
		tr := traceOf(targets, counts)

		if err := tr.LabelEndpoints(3); err != nil {
			t.Fatal(err)
		}
		want := map[string]int{"GET /a": 5, "GET /b": 2, OtherEndpoints: 4}
		if got := labelCounts(tr); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("trace order %v: labels = %v, want %v", targets, got, want)
		}
	}
}

func TestLabelEndpointsKeepsTheTimesAlignedWithTheRequests(t *testing.T) {
	tr := traceOf([]string{"/a", "/b"}, []int{2, 1})
	tr.stamps = []time.Duration{1, 2, 3}

	if err := tr.LabelEndpoints(5); err != nil {
		t.Fatal(err)
	}
	if len(tr.stamps) != 3 || tr.stamps[1] != 2 {
		t.Errorf("stamps = %v, want them untouched", tr.stamps)
	}
}

func TestLabelEndpointsNeedsRoomForTwoRows(t *testing.T) {
	// One row could only be "everything", which says nothing.
	for _, max := range []int{-1, 0, 1} {
		err := traceOf([]string{"/a"}, []int{1}).LabelEndpoints(max)
		if err == nil || !strings.Contains(err.Error(), "at least 2") {
			t.Errorf("LabelEndpoints(%d) error = %v, want one saying at least 2", max, err)
		}
	}
}

func TestOtherCannotBeMistakenForARealEndpoint(t *testing.T) {
	if strings.HasPrefix(OtherEndpoints, "GET ") || strings.Contains(OtherEndpoints, "/") {
		t.Errorf("%q looks like an endpoint: a real one could collide with it", OtherEndpoints)
	}
}
