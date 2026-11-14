package main

import (
	"strings"
	"testing"
	"time"
)

func TestFindSaysWhereTheTimeGrewAtTheFirstFailingRate(t *testing.T) {
	fast(t)
	// About 200 rps, and beyond it requests wait in the server for a slot: the
	// growth is in wait, the server's time.
	srv := capacityServer(t, 2, 10*time.Millisecond)

	code, out, stderr := exec(t, findArgs(srv.URL, "-start", "50", "-max", "1000", "-tolerance", "20", "-confirm", "0", "-json", "-")...)
	if code != 0 {
		t.Fatalf("exit %d, stdout:\n%s\nstderr:\n%s", code, out, stderr)
	}
	if !strings.Contains(out, `"phases"`) || !strings.Contains(out, `"wait_ms"`) {
		t.Errorf("the JSON has no phases for the trials:\n%s", out)
	}

	code, out, stderr = exec(t, findArgs(srv.URL, "-start", "50", "-max", "1000", "-tolerance", "20", "-confirm", "0")...)
	if code != 0 {
		t.Fatalf("exit %d, stdout:\n%s\nstderr:\n%s", code, out, stderr)
	}
	if strings.Contains(out, "LIMITED") {
		t.Skip("the generator's timers ran late in a trial, so the growth is rightly not blamed on the server")
	}
	if !strings.Contains(out, "where the time grew") || !strings.Contains(out, "most of it is wait") {
		t.Errorf("the report does not say the server's wait is what grew:\n%s", out)
	}
}

func TestFindRecordsNoPhasesWhenToldNotTo(t *testing.T) {
	fast(t)
	srv := capacityServer(t, 2, 10*time.Millisecond)
	code, out, stderr := exec(t, findArgs(srv.URL, "-phases=false", "-start", "50", "-max", "1000", "-tolerance", "20", "-confirm", "0", "-json", "-")...)
	if code != 0 {
		t.Fatalf("exit %d, stdout:\n%s\nstderr:\n%s", code, out, stderr)
	}
	if strings.Contains(out, `"phases"`) {
		t.Errorf("phases in the JSON although -phases=false:\n%s", out)
	}
}
