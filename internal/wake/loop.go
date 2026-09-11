package wake

import (
	"context"
	"log/slog"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/pace"
)

// Loop is the shape every worker in this service has: run a pass over a queue,
// wait, run another.
//
// It is one type rather than five copies of the same twenty lines because the
// waiting is the subtle part. Each loop has to widen its interval while idle so
// an empty service is not billed for asking an empty table the same question
// all day, reset that interval the moment work appears, and — the part that was
// missing — cut the wait short when another worker says there is something
// waiting. Getting one of those three wrong in one worker and not the others is
// how a service ends up fast at settling and slow at saying so.
type Loop struct {
	// Name is the component this loop is, and the value of the "component"
	// field on every line it logs.
	Name string
	// Topic is the wake signal that ends a wait early. Empty means this loop
	// is driven by its timer alone.
	Topic string
	// Interval is the polling floor: the wait after a pass that found work.
	Interval time.Duration
	// Bus may be nil, in which case the loop polls on its interval exactly as
	// it did before signals existed.
	Bus *Bus
	Log *slog.Logger
	// Fields are logged with the start line, for the settings worth knowing at
	// a glance — an endpoint, a limit, an account.
	Fields []any
	// Work runs one pass and reports whether it found anything to do.
	Work func(context.Context) bool
	// OnStop runs once the loop is finished, for workers holding resources —
	// open Horizon streams, say — that outlive a single pass.
	OnStop func()
}

// Run works the queue until the context is cancelled.
func (l Loop) Run(ctx context.Context) {
	interval := l.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	log := l.Log
	if log == nil {
		log = slog.Default()
	}

	fields := append([]any{"interval", interval, "wakes_on", l.Topic}, l.Fields...)
	log.Info(l.Name+" started", fields...)

	var idle pace.Backoff
	for {
		worked := l.Work(ctx)
		if ctx.Err() != nil {
			break
		}

		// How long this loop would sleep if nothing told it otherwise: the
		// interval while there is work, widening while there is not.
		delay := idle.Next(worked, interval)

		if l.Bus.Wait(ctx, l.Topic, delay) {
			// Signalled. Whatever woke it is work, so the next pass starts from
			// the base interval rather than from however far the loop had
			// drifted while it was quiet.
			idle.Reset()
			continue
		}
		if ctx.Err() != nil {
			break
		}
	}

	if l.OnStop != nil {
		l.OnStop()
	}
	log.Info(l.Name + " stopped")
}
