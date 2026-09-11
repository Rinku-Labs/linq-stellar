package pace

import (
	"testing"
	"time"
)

// The point of backing off is that an idle service stops asking. If misses did
// not widen the interval, nothing here would save any database time.
func TestIdleWidensThenHolds(t *testing.T) {
	var b Backoff
	base := time.Second

	for i, want := range []time.Duration{2, 4, 8, 16, 16, 16} {
		if got := b.Next(false, base); got != want*time.Second {
			t.Fatalf("after %d idle passes interval = %v, want %v", i+1, got, want*time.Second)
		}
	}
}

// Work has to restore the fast interval immediately: a busy service must not
// keep responding at the pace it settled into while it was quiet.
func TestWorkResetsImmediately(t *testing.T) {
	var b Backoff
	base := time.Second

	for i := 0; i < 5; i++ {
		b.Next(false, base)
	}
	if b.misses == 0 {
		t.Fatal("expected to have backed off")
	}

	if got := b.Next(true, base); got != base {
		t.Errorf("interval = %v after finding work, want %v", got, base)
	}
	if b.misses != 0 {
		t.Errorf("misses = %d after finding work, want 0", b.misses)
	}
}

// A signalled worker resets without having run a pass, so Reset has to stand on
// its own rather than only happening as a side effect of finding work.
func TestResetStandsAlone(t *testing.T) {
	var b Backoff
	for i := 0; i < 3; i++ {
		b.Next(false, time.Second)
	}
	b.Reset()
	if got := b.Next(false, time.Second); got != 2*time.Second {
		t.Errorf("interval after reset = %v, want the first backoff step", got)
	}
}

// The cap exists so a queue can never look stuck. Without it, repeated doubling
// would eventually put minutes or hours between passes.
func TestBackoffIsCapped(t *testing.T) {
	var b Backoff
	base := 30 * time.Second

	var last time.Duration
	for i := 0; i < 20; i++ {
		last = b.Next(false, base)
	}
	if last > MaxIdleInterval {
		t.Errorf("interval = %v, want it capped at %v", last, MaxIdleInterval)
	}
	if b.misses > 4 {
		t.Errorf("misses = %d, want it capped at 4", b.misses)
	}
}
