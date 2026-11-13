package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimm/Gmeter/internal/grpcdemo"
	"github.com/optimm/Gmeter/internal/results"
	"github.com/optimm/Gmeter/internal/runner"
)

func readResults(t *testing.T, path string) []runner.Sample {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows []runner.Sample
	if err := results.Read(f, func(s runner.Sample) { rows = append(rows, s) }); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestResultsSavesEveryRequestOfARun(t *testing.T) {
	srv := okServer(t)
	path := filepath.Join(t.TempDir(), "run.csv")

	code, _, stderr := exec(t, append(quickRun, "-results", path, srv.URL)...)

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	rows := readResults(t, path)
	if len(rows) != 30 {
		t.Fatalf("%d rows, want 30 (100/s for 300ms)", len(rows))
	}
	for _, r := range rows {
		if r.Status != 200 || r.Failure != "" || r.Finished < r.Sent || r.Sent < r.Due {
			t.Errorf("row %+v: want a 200 with due <= sent <= finished", r)
		}
	}
}

func TestResultsSavesAReplaysRequestsWithTheirEndpoint(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", timedTrace)
	path := filepath.Join(t.TempDir(), "replay.csv")

	code, _, stderr := execReplay(t, "-file", trace, "-methods", "GET,POST", "-quiet", "-results", path, srv.URL)

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	rows := readResults(t, path)
	if len(rows) != 3 || rows[0].Label == "" {
		t.Errorf("rows %+v, want 3, each with its endpoint as the label", rows)
	}
}

func TestResultsSavesAGRPCCallsStatusCode(t *testing.T) {
	addr, _ := echoServer(t, grpcdemo.ReflectionBoth)
	path := filepath.Join(t.TempDir(), "grpc.csv")

	grpcSaved(t, "-plaintext", "-call", grpcdemo.Say, "-rate", "50", "-duration", "200ms", "-quiet", "-results", path, addr)

	rows := readResults(t, path)
	if len(rows) != 10 || rows[0].Code != "OK" || rows[0].Status != 0 {
		t.Errorf("rows %+v, want 10 with the code OK", rows)
	}
}

func TestBadResultsFlagsAreUsageErrors(t *testing.T) {
	srv := okServer(t)
	t.Setenv(tokenEnv, "a-long-enough-password")
	good := filepath.Join(t.TempDir(), "x.csv")

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no such directory": {[]string{"-results", filepath.Join(t.TempDir(), "no", "such", "x.csv")}, "-results"},
		"with agents":       {[]string{"-results", good, "-agents", "127.0.0.1:1"}, "-agents"},
	} {
		code, _, stderr := exec(t, append(append(append([]string{}, quickRun...), tc.args...), srv.URL)...)
		if code != 2 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: exit code %d, stderr %q, want 2 and a message with %q", name, code, stderr, tc.want)
		}
	}
}
