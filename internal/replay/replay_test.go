package replay

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func mustJSONL(t *testing.T, in string) *Trace {
	t.Helper()
	tr, err := ParseJSONL(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseJSONL: %v", err)
	}
	return tr
}

func mustOffsets(t *testing.T, tr *Trace, speed float64, a pacer.Arrivals, seed uint64) []time.Duration {
	t.Helper()
	o, err := tr.Offsets(speed, a, seed)
	if err != nil {
		t.Fatalf("Offsets: %v", err)
	}
	return o
}

func TestParseJSONLReadsEveryField(t *testing.T) {
	tr := mustJSONL(t, `{"at":0.5,"method":"post","path":"/orders?id=7","headers":{"content-type":"application/json","X-Trace":"abc"},"body":"{\"a\":1}"}`+"\n")

	if len(tr.Requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(tr.Requests))
	}
	r := tr.Requests[0]
	if r.Method != "POST" {
		t.Errorf("Method = %q, want POST (upper-cased)", r.Method)
	}
	if r.Target != "/orders?id=7" {
		t.Errorf("Target = %q", r.Target)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q: header names must be canonicalised", got)
	}
	if got := r.Header.Get("X-Trace"); got != "abc" {
		t.Errorf("X-Trace = %q", got)
	}
	if string(r.Body) != `{"a":1}` {
		t.Errorf("Body = %q", r.Body)
	}
	if !tr.Timed() {
		t.Error("a trace whose lines have \"at\" is timed")
	}
}

func TestParseJSONLMethodAndFieldsAreOptional(t *testing.T) {
	tr := mustJSONL(t, `{"path":"/a"}`+"\n")
	r := tr.Requests[0]
	if r.Method != "" {
		t.Errorf("Method = %q, want empty (the runner's default method applies)", r.Method)
	}
	if tr.Timed() {
		t.Error("a trace with no \"at\" anywhere is not timed")
	}
}

func TestParseJSONLSkipsBlankLinesButKeepsLineNumbersHonest(t *testing.T) {
	_, err := ParseJSONL(strings.NewReader("\n{\"path\":\"/a\"}\n\n   \n{\"path\":\"nope\"}\n"))
	if err == nil || !strings.Contains(err.Error(), "line 5") {
		t.Fatalf("error = %v, want it to name line 5 (blank lines count)", err)
	}
}

func TestParseJSONLRejectsBadLines(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"not json", `{"path":"/a"}` + "\n" + `{"path":`, "line 2"},
		{"unknown field", `{"url":"/a"}`, `"url"`},
		{"missing path", `{"method":"GET"}`, "path"},
		{"path without a slash", `{"path":"a"}`, "start with"},
		{"absolute url", `{"path":"http://other.example/a"}`, "start with"},
		{"negative at", `{"at":-1,"path":"/a"}`, "at"},
		{"at on some lines only", `{"at":1,"path":"/a"}` + "\n" + `{"path":"/b"}`, "line 2"},
		{"at missing at first", `{"path":"/a"}` + "\n" + `{"at":1,"path":"/b"}`, "line 2"},
		{"trailing data", `{"path":"/a"} {"path":"/b"}`, "line 1"},
		{"empty", "", "no requests"},
		{"only blank lines", "\n\n", "no requests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseJSONL(strings.NewReader(tc.in))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestParseJSONLPutsRequestsInTimeOrderKeepingTiesInFileOrder(t *testing.T) {
	tr := mustJSONL(t, strings.Join([]string{
		`{"at":2,"path":"/late"}`,
		`{"at":1,"path":"/tie-first"}`,
		`{"at":0,"path":"/early"}`,
		`{"at":1,"path":"/tie-second"}`,
	}, "\n"))
	var got []string
	for _, r := range tr.Requests {
		got = append(got, r.Target)
	}
	want := []string{"/early", "/tie-first", "/tie-second", "/late"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestOffsetsAreRelativeToTheFirstRequestAndScaledBySpeed(t *testing.T) {
	tr := mustJSONL(t, strings.Join([]string{
		`{"at":100.0,"path":"/a"}`,
		`{"at":100.5,"path":"/b"}`,
		`{"at":102.0,"path":"/c"}`,
	}, "\n"))

	if got, want := mustOffsets(t, tr, 1, pacer.Even, 0), []time.Duration{0, ms(500), ms(2000)}; !slices.Equal(got, want) {
		t.Errorf("speed 1: %v, want %v", got, want)
	}
	if got, want := mustOffsets(t, tr, 2, pacer.Even, 0), []time.Duration{0, ms(250), ms(1000)}; !slices.Equal(got, want) {
		t.Errorf("speed 2 (twice as fast): %v, want %v", got, want)
	}
	if got, want := mustOffsets(t, tr, 0.5, pacer.Even, 0), []time.Duration{0, ms(1000), ms(4000)}; !slices.Equal(got, want) {
		t.Errorf("speed 0.5 (half as fast): %v, want %v", got, want)
	}
}

func TestOffsetsRefuseWhatTheyCannotHonour(t *testing.T) {
	timed := mustJSONL(t, `{"at":0,"path":"/a"}`)
	untimed := mustJSONL(t, `{"path":"/a"}`)
	for _, tc := range []struct {
		name  string
		tr    *Trace
		speed float64
		want  string
	}{
		{"untimed trace", untimed, 1, "no times"},
		{"zero speed", timed, 0, "speed"},
		{"negative speed", timed, -1, "speed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.tr.Offsets(tc.speed, pacer.Even, 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestKeepMethodsDropsTheRestAndTheirTimes(t *testing.T) {
	tr := mustJSONL(t, strings.Join([]string{
		`{"at":0,"method":"GET","path":"/a"}`,
		`{"at":1,"method":"POST","path":"/b"}`,
		`{"at":2,"path":"/c"}`, // no method: the runner's default (GET) applies, so it is kept
		`{"at":3,"method":"DELETE","path":"/d"}`,
		`{"at":4,"method":"head","path":"/e"}`,
	}, "\n"))

	dropped := tr.KeepMethods("get", "HEAD")

	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
	var got []string
	for _, r := range tr.Requests {
		got = append(got, r.Target)
	}
	if want := []string{"/a", "/c", "/e"}; !slices.Equal(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
	// The times of the dropped requests go with them: the rest keep their own.
	if got, want := mustOffsets(t, tr, 1, pacer.Even, 0), []time.Duration{0, 2 * time.Second, 4 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("offsets = %v, want %v", got, want)
	}
}
