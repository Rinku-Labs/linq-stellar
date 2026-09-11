// Package wake tells a worker there is something to do, instead of leaving it
// to ask.
//
// Every loop in this service is driven from the order table: a worker selects
// the rows in the state it owns, works them, and sleeps. That design is what
// makes a restart uneventful, and it is worth keeping. What it cost was
// latency. Because an idle loop backs its interval off (see internal/pace, and
// the database bill that motivated it), the first order after a quiet spell
// waited out whatever interval the loop had drifted to — up to two minutes —
// before anything looked at it. A deposit detected in one second then took
// another seventy-six to be paid out, and the merchant heard about it a minute
// after that. Nothing was broken; the work was simply sitting in a table
// nobody was due to read yet.
//
// So the sleep is now interruptible. A worker that creates work for another
// worker signals the topic that other worker waits on, and the wait ends at
// once. The polling interval stays exactly where it was, doing exactly what it
// did before — finding work that no signal announced, because the signaller
// crashed, the process restarted, or the row was written by a different
// deployment. Signals make the common path fast; the sweep is still the thing
// that makes it correct.
//
// A signal carries no payload deliberately. It means "look again", never "here
// is the order" — so a lost signal costs a delay rather than a settlement, and
// two signals for the same order are the same as one.
package wake

import (
	"context"
	"sync"
	"time"
)

// Topics. One per queue a worker drains, named for the work rather than for
// the worker, so the signaller does not have to know who is listening.
const (
	// Deposits: an order is newly awaiting a deposit and wants watching.
	Deposits = "deposits"
	// Payouts: a deposit was confirmed and the NGN disbursement is queued.
	Payouts = "payouts"
	// Chain: an on-chain job — a sweep, a refund, a reclaim — is queued.
	Chain = "chain"
	// Notify: a status transition was recorded and is owed downstream.
	Notify = "notify"
	// Pool: a provisioned deposit account was taken and wants replacing.
	Pool = "pool"
)

// Bus is a set of coalescing signals, one per topic.
//
// The zero value is not usable; call New.
type Bus struct {
	mu     sync.Mutex
	chans  map[string]chan struct{}
	bridge func(topic string)
}

// New returns an empty bus.
func New() *Bus {
	return &Bus{chans: map[string]chan struct{}{}}
}

// Bridge sets a hook called on every local Signal, for carrying it to other
// processes. Set it once, before the workers start.
//
// It exists because the loops and the thing that most often creates work for
// them — the API server accepting an order — run as separate deployments. See
// Listen.
func (b *Bus) Bridge(fn func(topic string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bridge = fn
}

// Signal wakes anything waiting on these topics, here and — through the bridge,
// if one is set — in other processes.
//
// Never blocks and never fails. A caller that has just moved money must not be
// held up, or made to handle an error, by the question of whether anyone was
// listening.
func (b *Bus) Signal(topics ...string) {
	if b == nil {
		return
	}
	for _, topic := range topics {
		b.signalLocal(topic)
		b.mu.Lock()
		bridge := b.bridge
		b.mu.Unlock()
		if bridge != nil {
			bridge(topic)
		}
	}
}

// SignalLocal wakes only this process. Used by the listener, which is
// delivering a signal that has already crossed the wire — bridging it again
// would send it straight back out.
func (b *Bus) SignalLocal(topics ...string) {
	if b == nil {
		return
	}
	for _, topic := range topics {
		b.signalLocal(topic)
	}
}

func (b *Bus) signalLocal(topic string) {
	select {
	// A full channel already means "look again", which is the whole message.
	// Queueing a second one would only make a worker run an extra empty pass.
	case b.chanFor(topic) <- struct{}{}:
	default:
	}
}

// chanFor returns the channel for a topic, creating it on first use.
//
// Buffered by one, so a signal sent while nobody is waiting is still delivered
// to the next waiter rather than dropped. That is the ordinary case: the worker
// is usually busy with the previous order, not parked on the channel.
func (b *Bus) chanFor(topic string) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch, ok := b.chans[topic]
	if !ok {
		ch = make(chan struct{}, 1)
		b.chans[topic] = ch
	}
	return ch
}

// Wait blocks until the topic is signalled, the timeout elapses, or the context
// is cancelled. It reports whether a signal is what ended the wait.
//
// A nil Bus waits out the timeout, so a worker built without one polls exactly
// as it did before.
func (b *Bus) Wait(ctx context.Context, topic string, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()

	var signal <-chan struct{}
	if b != nil {
		signal = b.chanFor(topic)
	}

	select {
	case <-ctx.Done():
		return false
	case <-signal:
		return true
	case <-t.C:
		return false
	}
}
