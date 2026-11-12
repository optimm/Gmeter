package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// phasesSaved runs gmeter against url with the given flags and -json, and
// returns what it printed, the names of the phases it saved and the file.
func phasesSaved(t *testing.T, run func(*testing.T, ...string) (int, string, string), url string, flags ...string) (stdout string, saved []string, raw string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "run.json")
	code, stdout, stderr := run(t, append(flags, "-quiet", "-json", out, url)...)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Phases []struct {
			Phase string `json:"phase"`
			Count uint64 `json:"count"`
		} `json:"phases"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, p := range got.Phases {
		saved = append(saved, p.Phase)
	}
	return stdout, saved, string(b)
}

func TestARunSplitsItsLatencyIntoPhasesByDefault(t *testing.T) {
	srv := okServer(t)

	stdout, saved, _ := phasesSaved(t, exec, srv.URL, "-rate", "100", "-duration", "300ms", "-workers", "4")

	if want := "queue,connect,send,wait,transfer"; strings.Join(saved, ",") != want {
		t.Errorf("saved phases = %v, want %s: an IP address over plain HTTP has no dns or tls, and every connection was opened once", saved, want)
	}
	for _, want := range []string{"where the time goes", "queue", "wait", "transfer"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
}

func TestPhasesFalseLeavesThemOut(t *testing.T) {
	srv := okServer(t)

	stdout, saved, raw := phasesSaved(t, exec, srv.URL, "-phases=false", "-rate", "100", "-duration", "300ms")

	if len(saved) != 0 || strings.Contains(stdout, "where the time goes") || strings.Contains(raw, `"phases"`) {
		t.Errorf("-phases=false: saved %v, want no phases in the report or the file:\n%s", saved, stdout)
	}
}

func TestAReplaySplitsItsLatencyIntoPhasesToo(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", timedTrace)

	stdout, saved, _ := phasesSaved(t, execReplay, srv.URL, "-file", trace, "-methods", "all", "-workers", "4")
	if strings.Join(saved, ",") != "queue,connect,send,wait,transfer" || !strings.Contains(stdout, "where the time goes") {
		t.Errorf("replay saved phases %v, want queue,connect,send,wait,transfer:\n%s", saved, stdout)
	}

	stdout, saved, _ = phasesSaved(t, execReplay, srv.URL, "-phases=false", "-file", trace, "-methods", "all")
	if len(saved) != 0 || strings.Contains(stdout, "where the time goes") {
		t.Errorf("replay with -phases=false saved %v:\n%s", saved, stdout)
	}
}
