package pacer

import (
	"context"
	"time"
)

// minSleep is the shortest sleep worth asking the OS for. Below it the timer's
// own overshoot is as long as the sleep, so that stretch is spun through.
const minSleep = 50 * time.Microsecond

// WaitUntil blocks until t or until ctx is done, whichever comes first. It
// returns nil if t was reached (or already in the past) and ctx.Err()
// otherwise. A past deadline returns immediately without yielding, so a
// generator that has fallen behind catches up by sending back-to-back.
//
// With spin <= 0 it is one plain timer. That wakes late, by an amount the OS
// decides: measured on one Mac, the median was about 2ms for sleeps of 10ms or
// more, which would be charged to every request's latency.
//
// With spin > 0 it sleeps until the last `spin` before t and busy-waits the
// rest, trading CPU for precision. It does not sleep straight to t-spin: a
// timer that wakes late would overshoot t itself. Instead each sleep covers
// only half of the time still to cover, and the remainder is re-read after
// every wake-up, so a late wake-up eats into slack that is still there. The
// price is up to `spin` of one core's time per wait: at rate r that is at most
// min(1, spin*r) of a core.
func WaitUntil(ctx context.Context, t time.Time, spin time.Duration) error {
	if time.Until(t) <= 0 {
		return nil
	}
	if spin <= 0 {
		return sleep(ctx, nil, time.Until(t))
	}

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		d := coarseSleep(time.Until(t), spin)
		if d == 0 {
			break
		}
		if err := sleep(ctx, timer, d); err != nil {
			return err
		}
	}
	done := ctx.Done()
	for time.Until(t) > 0 {
		select {
		case <-done:
			return ctx.Err()
		default:
		}
	}
	return nil
}

// coarseSleep is how long to sleep when rem is left until the deadline and the
// last spin is to be busy-waited. Zero means it is time to spin.
//
// It never sleeps into the spin window, so the busy-wait is the window and no
// more, which is what bounds the CPU cost. Far from the window it sleeps only
// half of the slack, so a timer that wakes late still lands before the
// deadline. When the slack is too small for another halving to be worth a
// wake-up it sleeps all of it.
func coarseSleep(rem, spin time.Duration) time.Duration {
	slack := rem - spin
	switch {
	case slack < minSleep:
		return 0
	case slack < 2*minSleep:
		return slack
	default:
		return slack / 2
	}
}

// sleep blocks for d or until ctx is done. A nil timer makes it allocate one;
// otherwise the caller's timer is reused.
func sleep(ctx context.Context, timer *time.Timer, d time.Duration) error {
	if timer == nil {
		timer = time.NewTimer(d)
		defer timer.Stop()
	} else {
		timer.Reset(d)
	}
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// maxSpinShare is the largest share of the gap between two requests that may
// be spent busy-waiting: a quarter, so spinning costs at most about a quarter
// of one core at any rate.
const maxSpinShare = 4

// CapSpin limits a spin window to a quarter of gap, the time between the
// request being waited for and the one before it. A timer's overshoot shrinks
// with the length of the sleep, so a short gap needs a short window, and a
// window as long as the gap would just be a busy loop.
//
// It returns zero (no spinning, a plain timer) when there is nothing to wait
// for (gap not positive), when spinning is off, or when the capped window is
// shorter than minSleep. In that last case, above about 5000 requests a second,
// the scheduler could never fit a sleep in front of the window and would
// busy-loop the whole gap; a plain timer is already within tens of
// microseconds at those rates.
func CapSpin(spin, gap time.Duration) time.Duration {
	if gap <= 0 || spin <= 0 {
		return 0
	}
	if w := min(spin, gap/maxSpinShare); w >= minSleep {
		return w
	}
	return 0
}
