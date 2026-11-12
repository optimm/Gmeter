package pacer

import (
	"strings"
	"testing"
	"time"
)

func TestReplayDueTimesAreStartPlusOffsets(t *testing.T) {
	start := time.Unix(1000, 0)
	offsets := []time.Duration{0, 10 * time.Millisecond, 10 * time.Millisecond, 2500 * time.Millisecond}
	r, err := NewReplay(start, offsets)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range offsets {
		at, stage, ok := r.Next()
		if !ok {
			t.Fatalf("request %d: schedule ended early", i)
		}
		if got := at.Sub(start); got != want {
			t.Errorf("request %d: due %v after start, want %v", i, got, want)
		}
		if stage != 0 {
			t.Errorf("request %d: stage %d, want 0: a replay has a single stage", i, stage)
		}
	}
	if _, _, ok := r.Next(); ok {
		t.Error("Next reported a request after the last one")
	}
	if _, _, ok := r.Next(); ok {
		t.Error("Next must keep reporting the end once it has been reached")
	}
}

func TestReplayDurationIsTheLastOffset(t *testing.T) {
	r, err := NewReplay(time.Time{}, []time.Duration{0, time.Second, 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Duration(); got != 3*time.Second {
		t.Errorf("Duration = %v, want 3s", got)
	}
}

func TestReplayRejectsBadOffsets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offsets []time.Duration
		want    string
	}{
		{"none", nil, "no requests"},
		{"negative", []time.Duration{0, -time.Millisecond}, "negative"},
		{"out of order", []time.Duration{0, 2 * time.Second, time.Second}, "order"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewReplay(time.Time{}, tc.offsets)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestReplayDoesNotKeepTheCallersSlice(t *testing.T) {
	offsets := []time.Duration{0, time.Second}
	r, err := NewReplay(time.Time{}, offsets)
	if err != nil {
		t.Fatal(err)
	}
	offsets[1] = time.Hour // a caller reusing its slice must not move due times
	r.Next()
	at, _, _ := r.Next()
	if got := at.Sub(time.Time{}); got != time.Second {
		t.Errorf("second request due at %v, want 1s", got)
	}
}
