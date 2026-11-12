package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// answeringServer answers each path with the status it is given.
func answeringServer(t *testing.T, statuses map[string]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s, ok := statuses[r.URL.Path]; ok {
			w.WriteHeader(s)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// loggedRequests: the log says every one of them was answered 200 except /nolog,
// which has no logged status. The server answers /ok 200, /same 204 (the same
// class), /gone 404 and /boom 503.
func loggedRequests() []Request {
	return []Request{
		{Target: "/ok", Label: "/ok", Logged: 200},
		{Target: "/same", Label: "/same", Logged: 200},
		{Target: "/gone", Label: "/gone", Logged: 200},
		{Target: "/boom", Label: "/boom", Logged: 200},
		{Target: "/nolog", Label: "/nolog"},
	}
}

func runLogged(t *testing.T, cfg Config) *Result {
	t.Helper()
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRunComparesEachResponsesClassWithTheLoggedOne(t *testing.T) {
	srv := answeringServer(t, map[string]int{"/same": 204, "/gone": 404, "/boom": 503})
	cfg := baseConfig(srv.URL) // 100 requests: 20 of each of the 5
	cfg.Requests = loggedRequests()

	res := runLogged(t, cfg)

	if res.Logged.Compared != 80 {
		t.Errorf("Compared = %d, want 80: the 4 requests with a logged status, 20 each; /nolog has nothing to compare", res.Logged.Compared)
	}
	// Most common first; a tie by class.
	want := []StatusChange{{Logged: 2, Got: 4, Count: 20}, {Logged: 2, Got: 5, Count: 20}}
	if len(res.Logged.Changes) != len(want) || res.Logged.Changes[0] != want[0] || res.Logged.Changes[1] != want[1] {
		t.Errorf("Changes = %v, want %v", res.Logged.Changes, want)
	}
	if got := res.Logged.Differ(); got != 40 {
		t.Errorf("Differ = %d, want 40", got)
	}
}

func TestRunCountsLoggedStatusDifferencesPerLabel(t *testing.T) {
	srv := answeringServer(t, map[string]int{"/same": 204, "/gone": 404, "/boom": 503})
	cfg := baseConfig(srv.URL)
	cfg.Requests = loggedRequests()

	res := runLogged(t, cfg)

	for label, want := range map[string][2]uint64{ // compared, differ
		"/ok": {20, 0}, "/same": {20, 0}, "/gone": {20, 20}, "/boom": {20, 20}, "/nolog": {0, 0},
	} {
		l := labelNamed(t, res, label)
		if l.LoggedCompared != want[0] || l.LoggedDiffer != want[1] {
			t.Errorf("%s: compared %d, differ %d, want %d and %d", label, l.LoggedCompared, l.LoggedDiffer, want[0], want[1])
		}
	}
}

func TestRunDoesNotCompareRequestsThatGotNoResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack() // close without answering
		if err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	cfg := baseConfig(srv.URL)
	cfg.Requests = []Request{{Target: "/x", Label: "/x", Logged: 200}, {Target: "/y", Label: "/y", Logged: 200}}

	res := runLogged(t, cfg)

	if res.Logged.Compared != 0 || len(res.Logged.Changes) != 0 {
		t.Errorf("Logged = %+v, want nothing compared: no request was answered", res.Logged)
	}
}

func TestRunWithoutLoggedStatusesComparesNothing(t *testing.T) {
	srv := answeringServer(t, map[string]int{"/b": 500})
	cfg := baseConfig(srv.URL)
	cfg.Requests = labelled("/a", "/b")

	res := runLogged(t, cfg)

	if res.Logged.Compared != 0 || res.Logged.Changes != nil || res.Logged.Differ() != 0 {
		t.Errorf("Logged = %+v, want the zero value", res.Logged)
	}
}

func TestRunListsTheMostCommonChangeFirst(t *testing.T) {
	srv := answeringServer(t, map[string]int{"/gone": 404, "/boom": 503})
	cfg := baseConfig(srv.URL)
	// 100 requests cycling through three: 4xx a third of the time, 5xx two thirds.
	cfg.Requests = []Request{{Target: "/gone", Logged: 200}, {Target: "/boom", Logged: 200}, {Target: "/boom", Logged: 200}}

	res := runLogged(t, cfg)

	if len(res.Logged.Changes) != 2 || res.Logged.Changes[0].Got != 5 || res.Logged.Changes[1].Got != 4 {
		t.Fatalf("Changes = %v, want 2xx->5xx first (twice as common), then 2xx->4xx", res.Logged.Changes)
	}
	if res.Logged.Changes[0].Count <= res.Logged.Changes[1].Count {
		t.Errorf("counts %d then %d, want the first larger", res.Logged.Changes[0].Count, res.Logged.Changes[1].Count)
	}
}
