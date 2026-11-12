package replay

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
)

func mustCLF(t *testing.T, lines ...string) *Trace {
	t.Helper()
	tr, err := ParseCLF(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("ParseCLF: %v", err)
	}
	return tr
}

// clf is a common-log-format line at the given second of a fixed minute.
func clf(sec int, request string) string {
	return fmt.Sprintf(`10.0.0.1 - frank [10/Oct/2023:13:55:%02d +0000] "%s" 200 2326`, sec, request)
}

func TestParseCLFReadsCommonAndCombinedLines(t *testing.T) {
	tr := mustCLF(t,
		`127.0.0.1 - - [10/Oct/2023:13:55:36 -0700] "GET /apache_pb.gif?size=2 HTTP/1.0" 200 2326`,
		`203.0.113.9 - bob [10/Oct/2023:13:55:37 -0700] "POST /orders HTTP/1.1" 201 17 "https://example.com/cart" "Mozilla/5.0 (X11; Linux)"`,
	)
	if len(tr.Requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(tr.Requests))
	}
	if r := tr.Requests[0]; r.Method != "GET" || r.Target != "/apache_pb.gif?size=2" {
		t.Errorf("first = %s %s", r.Method, r.Target)
	}
	if r := tr.Requests[1]; r.Method != "POST" || r.Target != "/orders" {
		t.Errorf("second = %s %s", r.Method, r.Target)
	}
	if len(tr.Requests[1].Body) != 0 {
		t.Error("an access log has no request bodies, so none may be invented")
	}
	if !tr.Timed() {
		t.Error("access log lines carry timestamps")
	}
}

func TestParseCLFCountsLinesItCannotUseInsteadOfFailing(t *testing.T) {
	tr := mustCLF(t,
		clf(1, "GET /a HTTP/1.1"),
		`this is not a log line`,
		`10.0.0.1 - - [10/Oct/2023:13:55:02 +0000] "-" 408 0`, // a connection that never sent a request
		clf(3, "GET http://proxy.example/x HTTP/1.1"),         // an absolute-form target: no path to replay
		`10.0.0.1 - - [not a date] "GET /b HTTP/1.1" 200 1`,
		clf(4, "GET /c HTTP/1.1"),
	)
	if len(tr.Requests) != 2 {
		t.Fatalf("got %d requests, want the 2 good ones", len(tr.Requests))
	}
	if tr.Unparsed != 4 {
		t.Errorf("Unparsed = %d, want 4", tr.Unparsed)
	}
}

func TestParseCLFFailsWhenNothingInTheFileIsALogLine(t *testing.T) {
	_, err := ParseCLF(strings.NewReader("hello\nworld\n"))
	if err == nil || !strings.Contains(err.Error(), "common log format") {
		t.Fatalf("error = %v, want it to say nothing looked like the common log format", err)
	}
	_, err = ParseCLF(strings.NewReader(""))
	if err == nil {
		t.Fatal("an empty file must be an error")
	}
}

func TestParseCLFUsesTheTimezoneInTheTimestamp(t *testing.T) {
	// 13:55:36 at -0700 is 20:55:36 UTC; 20:55:37 at +0000 is one second later.
	tr := mustCLF(t,
		`h - - [10/Oct/2023:13:55:36 -0700] "GET /first HTTP/1.1" 200 1`,
		`h - - [10/Oct/2023:20:55:37 +0000] "GET /second HTTP/1.1" 200 1`,
	)
	offsets := mustOffsets(t, tr, 1, pacer.Even, 0)
	if want := []time.Duration{0, time.Second}; !slices.Equal(offsets, want) {
		t.Errorf("offsets = %v, want %v", offsets, want)
	}
}

func TestParseCLFPutsRequestsInTimeOrder(t *testing.T) {
	// Servers log when a request finishes, so lines are slightly out of order.
	tr := mustCLF(t, clf(5, "GET /b HTTP/1.1"), clf(4, "GET /a HTTP/1.1"), clf(5, "GET /c HTTP/1.1"))
	var got []string
	for _, r := range tr.Requests {
		got = append(got, r.Target)
	}
	if want := []string{"/a", "/b", "/c"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v (ties keep file order)", got, want)
	}
}

func TestEvenSpreadPutsTheRequestsOfASecondEvenlyInsideIt(t *testing.T) {
	// Second 0 has four requests, second 1 none, second 2 has two.
	tr := mustCLF(t,
		clf(0, "GET /a HTTP/1.1"), clf(0, "GET /b HTTP/1.1"), clf(0, "GET /c HTTP/1.1"), clf(0, "GET /d HTTP/1.1"),
		clf(2, "GET /e HTTP/1.1"), clf(2, "GET /f HTTP/1.1"),
	)
	got := mustOffsets(t, tr, 1, pacer.Even, 0)
	want := []time.Duration{0, ms(250), ms(500), ms(750), 2 * time.Second, 2*time.Second + ms(500)}
	if !slices.Equal(got, want) {
		t.Errorf("offsets = %v, want %v", got, want)
	}
}

func TestPoissonSpreadKeepsEachSecondsCountInsideThatSecond(t *testing.T) {
	var lines []string
	perSecond := []int{30, 0, 12, 50} // request counts for seconds 0..3
	for sec, n := range perSecond {
		for i := 0; i < n; i++ {
			lines = append(lines, clf(sec, fmt.Sprintf("GET /s%d/%d HTTP/1.1", sec, i)))
		}
	}
	tr := mustCLF(t, lines...)

	offsets := mustOffsets(t, tr, 1, pacer.Poisson, 42)

	if !slices.IsSorted(offsets) {
		t.Fatal("offsets must be in time order")
	}
	counts := make([]int, len(perSecond))
	for _, o := range offsets {
		counts[int(o/time.Second)]++
	}
	if !slices.Equal(counts, perSecond) {
		t.Errorf("per-second counts = %v, want %v: the log's counts must survive the spreading", counts, perSecond)
	}
	// Not secretly even: with 30 requests in one second, exact spacing would put
	// every gap at 1/30s.
	gaps := map[time.Duration]bool{}
	for i := 1; i < 30; i++ {
		gaps[offsets[i]-offsets[i-1]] = true
	}
	if len(gaps) < 10 {
		t.Errorf("only %d distinct gaps among 29: the spread is not random", len(gaps))
	}
}

func TestPoissonSpreadFollowsTheSeed(t *testing.T) {
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, clf(0, fmt.Sprintf("GET /%d HTTP/1.1", i)))
	}
	tr := mustCLF(t, lines...)

	a := mustOffsets(t, tr, 1, pacer.Poisson, 7)
	b := mustOffsets(t, tr, 1, pacer.Poisson, 7)
	c := mustOffsets(t, tr, 1, pacer.Poisson, 8)
	if !slices.Equal(a, b) {
		t.Error("the same seed must give the same offsets")
	}
	if slices.Equal(a, c) {
		t.Error("a different seed must give different offsets")
	}
}

func TestSpreadingHappensBeforeSpeedScaling(t *testing.T) {
	tr := mustCLF(t, clf(0, "GET /a HTTP/1.1"), clf(0, "GET /b HTTP/1.1"), clf(1, "GET /c HTTP/1.1"))
	got := mustOffsets(t, tr, 2, pacer.Even, 0)
	want := []time.Duration{0, ms(250), ms(500)} // 0, 0.5s, 1s at speed 1
	if !slices.Equal(got, want) {
		t.Errorf("offsets = %v, want %v", got, want)
	}
}
