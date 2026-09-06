// Package settle turns an observed on-chain deposit into a settled order.
//
// Deposit detection is deliberately redundant. A Horizon stream sees a payment
// in about a second; a polling sweep catches whatever the stream dropped across
// a reconnect. Redundancy is the point — a stream that silently misses an event
// means a payer who paid and got nothing — but it means two goroutines can
// reach the same order at the same instant.
//
// Everything therefore funnels through Deposits.Record, which claims the order
// with a compare-and-swap before anything downstream happens. A detector that
// queues a payout without claiming first will eventually pay an order twice.
// One door to the money, no exceptions.
package settle

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/uselinq/linq-stellar/internal/stellar"
	"github.com/uselinq/linq-stellar/internal/store"
	"gorm.io/gorm"
)

// Payouts queues an order for NGN disbursement once its deposit is confirmed.
type Payouts interface {
	Enqueue(orderID string) error
}

// Deposits records confirmed deposits and hands them to payout.
type Deposits struct {
	DB      *gorm.DB
	Chain   *stellar.Client
	Payouts Payouts
	Log     *slog.Logger
}

// Record claims an order for a confirmed deposit and queues its payout.
//
// Returns true only for the caller that won the claim. A false return is the
// ordinary outcome when the other detector arrived first: nothing has gone
// wrong and the caller should simply stop.
//
// The amount recorded is what actually arrived, never what was requested.
// Deposits are manual — a payer sends what they like — so storing the requested
// figure would make the order permanently misreport itself, and on a
// manual-deposit order it is also the number the payout is computed from.
func (d *Deposits) Record(order *store.Order, onChain float64, info stellar.DepositInfo) (bool, error) {
	payable, ngn, err := Reconcile(onChain, order.FeeUSDC, order.Rate)
	if err != nil {
		// Not an error worth logging loudly: "nothing has arrived yet" is the
		// normal state of an order being watched.
		return false, nil
	}

	now := time.Now().UTC()
	fields := map[string]any{
		"amount_usdc":  payable,
		"deposited_at": &now,
	}
	// A manual deposit reconciles the payout to what landed. A precomputed one
	// keeps the NGN it was quoted at, because the integrator has already
	// promised that figure to their user.
	if order.ManualDeposit {
		fields["amount_ngn"] = ngn
	}
	if info.TxHash != "" {
		fields["deposit_tx_hash"] = info.TxHash
	}
	if info.From != "" {
		fields["deposit_from"] = info.From
	}

	won, err := store.ClaimStatusWith(d.DB, order.ID,
		store.StateAwaitingDeposit, store.StateDepositDetected, fields)
	if err != nil {
		return false, fmt.Errorf("settle: claim deposit for %s: %w", order.ID, err)
	}
	if !won {
		return false, nil
	}

	d.Log.Info("deposit confirmed",
		"order", order.ID, "usdc", payable, "from", info.From, "tx", info.TxHash)

	// From here the order is claimed but no payout job exists yet. If queueing
	// fails the claim is handed back rather than left stranded in a state
	// nothing is working on — the detectors will find it again next sweep.
	if err := d.Payouts.Enqueue(order.ID); err != nil {
		d.Log.Error("payout enqueue failed, releasing claim",
			"order", order.ID, "error", err)
		if relErr := store.ReleaseStatus(d.DB, order.ID,
			store.StateDepositDetected, store.StateAwaitingDeposit); relErr != nil {
			// Now it is stuck: claimed, unqueued, and not released. Say so
			// loudly, because it needs a human before the payer does.
			d.Log.Error("could not release claim; order is stranded",
				"order", order.ID, "error", relErr)
		}
		return false, fmt.Errorf("settle: enqueue payout for %s: %w", order.ID, err)
	}

	if _, err := store.ClaimStatus(d.DB, order.ID,
		store.StateDepositDetected, store.StatePayoutQueued); err != nil {
		// The job is already queued, so the payout will still happen; only the
		// recorded state is behind. Worth knowing about, not worth failing.
		d.Log.Error("could not advance to payout_queued", "order", order.ID, "error", err)
	}
	return true, nil
}

// Expire gives up on an order whose deposit never arrived.
//
// No money moved, so this is an ordinary ending rather than a failure. The
// deposit account still holds sponsored reserves, which the reclaim worker
// returns to the sponsor.
func (d *Deposits) Expire(order *store.Order) error {
	won, err := store.ClaimStatus(d.DB, order.ID,
		store.StateAwaitingDeposit, store.StateExpired)
	if err != nil {
		return fmt.Errorf("settle: expire %s: %w", order.ID, err)
	}
	if won {
		d.Log.Info("order expired with no deposit",
			"order", order.ID, "address", order.DepositAddress)
	}
	return nil
}

// Reconcile is the money maths for a deposit: given what landed, what is
// payable and what is it worth in NGN?
//
// Split out so it can be exercised without Horizon or a database. It is the
// only part of deposit handling that decides an amount of money, and it is
// worth being able to prove.
//
// An error means there is nothing to pay out yet, which is the normal
// keep-waiting signal rather than a failure.
func Reconcile(balance, fee, rate float64) (payable, ngn float64, err error) {
	if balance <= 0 {
		return 0, 0, fmt.Errorf("settle: no balance")
	}
	payable = balance - fee
	if payable <= 0 {
		return 0, 0, fmt.Errorf("settle: deposit does not cover the fee")
	}
	return payable, payable * rate, nil
}
