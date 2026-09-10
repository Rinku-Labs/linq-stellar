// Package pace slows idle polling loops down.
package pace

import "time"

// Long enough that an idle service is nearly silent against the database,
// short enough that the first order after a quiet spell is not left waiting.
// MaxIdleInterval caps how far a quiet worker backs off.
const MaxIdleInterval = 2 * time.Minute

// Backoff widens a worker's polling interval while there is nothing to do.
//
// These loops are queues that are empty most of the time. Polling them at a
// fixed few seconds means the database is asked "is there anything yet?"
// tens of thousands of times a day, almost always to be told no — which is
// most of what a small Postgres instance ends up billing for.
//
// Work resets the interval immediately, so a busy service still behaves as if
// it were polling constantly. Only idleness is slowed down.
type Backoff struct {
	misses int
}

// Next widens or resets the ticker based on whether the last pass found work.
func (b *Backoff) Next(worked bool, t *time.Ticker, base time.Duration) {
	if worked {
		if b.misses > 0 {
			b.misses = 0
			t.Reset(base)
		}
		return
	}

	// Doubling, capped at four steps, so the interval grows 2x, 4x, 8x, 16x and
	// then holds. Unbounded doubling would eventually make a queue look stuck.
	if b.misses < 4 {
		b.misses++
	}
	d := base << b.misses
	if d > MaxIdleInterval {
		d = MaxIdleInterval
	}
	t.Reset(d)
}
