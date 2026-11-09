package main

import (
	"context"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/Rinku-Labs/linq-stellar/internal/wake"
)

func woken(bus *wake.Bus, topic string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	return bus.Wait(ctx, topic, 50*time.Millisecond)
}

// A payout the worker hands back, still in flight or after a failure, must not
// wake the worker again at once. It did, and an order waiting on the bank was
// asked about some twenty times a second, writing two status rows each time.
func TestAPayoutHandedBackDoesNotWakeThePayoutWorker(t *testing.T) {
	bus := wake.New()
	signal := waker(bus)

	signal("o1", store.StatePayoutProcessing, store.StatePayoutQueued)
	if woken(bus, wake.Payouts) {
		t.Errorf("payout_processing -> payout_queued woke the payout worker")
	}

	signal("o1", store.StateDepositDetected, store.StatePayoutQueued)
	if !woken(bus, wake.Payouts) {
		t.Errorf("a new payout (deposit_detected -> payout_queued) did not wake the payout worker")
	}
}
