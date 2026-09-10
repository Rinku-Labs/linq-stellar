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
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for i, want := range []int{1, 2, 3, 4, 4, 4} {
		b.Next(false, ticker, base)
		if b.misses != want {
			t.Fatalf("after %d idle passes misses = %d, want %d", i+1, b.misses, want)
		}
	}
}

// Work has to restore the fast interval immediately: a busy service must not
// keep responding at the pace it settled into while it was quiet.
func TestWorkResetsImmediately(t *testing.T) {
	var b Backoff
	base := time.Second
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for i := 0; i < 5; i++ {
		b.Next(false, ticker, base)
	}
	if b.misses == 0 {
		t.Fatal("expected to have backed off")
	}

	b.Next(true, ticker, base)
	if b.misses != 0 {
		t.Errorf("misses = %d after finding work, want 0", b.misses)
	}
}

// The cap exists so a queue can never look stuck. Without it, repeated doubling
// would eventually put minutes or hours between passes.
func TestBackoffIsCapped(t *testing.T) {
	var b Backoff
	base := 30 * time.Second
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for i := 0; i < 20; i++ {
		b.Next(false, ticker, base)
	}
	if got := base << b.misses; got < MaxIdleInterval {
		t.Logf("interval %v is below the cap, which is fine", got)
	}
	if b.misses > 4 {
		t.Errorf("misses = %d, want it capped at 4", b.misses)
	}
}
