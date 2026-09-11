package wake

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The whole point. A worker parked on a long interval has to come back the
// moment there is work, or the interval is the service's latency — which is
// what put seventy-six seconds between a confirmed deposit and its payout.
func TestSignalEndsALongWait(t *testing.T) {
	bus := New()
	started := time.Now()

	go func() {
		time.Sleep(20 * time.Millisecond)
		bus.Signal(Payouts)
	}()

	if !bus.Wait(context.Background(), Payouts, 30*time.Second) {
		t.Fatal("wait ended without a signal")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("waited %v; the signal should have cut the interval short", elapsed)
	}
}

// A signal sent while the worker is busy must still be there when it next
// waits. Dropping it would mean an order sitting until the polling backstop
// noticed — exactly the delay this package exists to remove.
func TestSignalSentBeforeTheWaitIsKept(t *testing.T) {
	bus := New()
	bus.Signal(Notify)

	if !bus.Wait(context.Background(), Notify, time.Second) {
		t.Fatal("a signal sent before the wait was lost")
	}
}

// Signals coalesce: they say "look again", never "here is an order". Ten
// deposits in a burst are one wake-up, and the pass that follows finds all ten.
func TestSignalsCoalesce(t *testing.T) {
	bus := New()
	for i := 0; i < 10; i++ {
		bus.Signal(Chain)
	}
	if !bus.Wait(context.Background(), Chain, time.Second) {
		t.Fatal("expected the first wait to be satisfied")
	}
	if bus.Wait(context.Background(), Chain, 20*time.Millisecond) {
		t.Error("a second wait was satisfied; ten signals should collapse into one")
	}
}

// Topics are separate queues. Waking the payout worker must not wake the
// notifier, or every signal costs every loop a pass.
func TestTopicsAreIndependent(t *testing.T) {
	bus := New()
	bus.Signal(Payouts)

	if bus.Wait(context.Background(), Notify, 20*time.Millisecond) {
		t.Error("a payout signal woke the notifier")
	}
	if !bus.Wait(context.Background(), Payouts, time.Second) {
		t.Error("the payout signal was consumed by the wrong topic")
	}
}

// Without a signal the wait is the polling interval, unchanged. The backoff is
// still what covers work nobody announced.
func TestWaitFallsBackToTheInterval(t *testing.T) {
	bus := New()
	started := time.Now()
	if bus.Wait(context.Background(), Deposits, 30*time.Millisecond) {
		t.Fatal("reported a signal when none was sent")
	}
	if elapsed := time.Since(started); elapsed < 25*time.Millisecond {
		t.Errorf("returned after %v, before the interval elapsed", elapsed)
	}
}

// A worker built without a bus — a test, a local run — must still poll rather
// than panic.
func TestNilBusStillPolls(t *testing.T) {
	var bus *Bus
	bus.Signal(Payouts) // must not panic
	if bus.Wait(context.Background(), Payouts, 10*time.Millisecond) {
		t.Error("a nil bus reported a signal")
	}
}

// Shutdown has to end a wait too, or every loop holds the process open for as
// long as its interval.
func TestCancelEndsTheWait(t *testing.T) {
	bus := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if bus.Wait(ctx, Payouts, time.Hour) {
		t.Error("a cancelled wait reported a signal")
	}
}

// The bridge is how a signal reaches the other deployment. It must fire for
// every local signal, and must not fire for one that arrived from outside —
// that would bounce every signal back where it came from.
func TestBridgeCarriesLocalSignalsOnly(t *testing.T) {
	bus := New()
	var bridged []string
	var mu sync.Mutex
	bus.Bridge(func(topic string) {
		mu.Lock()
		defer mu.Unlock()
		bridged = append(bridged, topic)
	})

	bus.Signal(Deposits)
	bus.SignalLocal(Payouts)

	mu.Lock()
	defer mu.Unlock()
	if len(bridged) != 1 || bridged[0] != Deposits {
		t.Errorf("bridged %v, want only the locally raised signal", bridged)
	}
}

// A loop must work its queue on a signal, not merely wake up: the regression
// this replaced was a worker that was awake and still did not look.
func TestLoopRunsAPassPerSignal(t *testing.T) {
	bus := New()
	var passes atomic.Int32
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		defer close(done)
		Loop{
			Name:     "test loop",
			Topic:    Chain,
			Interval: time.Hour, // only a signal can move this along
			Bus:      bus,
			Log:      slog.New(slog.DiscardHandler),
			Work: func(context.Context) bool {
				passes.Add(1)
				return false
			},
		}.Run(ctx)
	}()

	// One pass runs on entry; each signal must add another.
	for i := 0; i < 3; i++ {
		waitFor(t, func() bool { return passes.Load() >= int32(i+1) })
		bus.Signal(Chain)
	}
	waitFor(t, func() bool { return passes.Load() >= 4 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop when its context was cancelled")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for the loop")
}
