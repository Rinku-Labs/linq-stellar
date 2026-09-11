package payout

import (
	"context"
	"log/slog"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/Rinku-Labs/linq-stellar/internal/wake"
	"gorm.io/gorm"
)

const (
	defaultInterval = 5 * time.Second
	defaultBatch    = 50
	// maxAttempts before an order is sent down the refund path. Three is
	// enough to ride out a provider blip without leaving a payer waiting on a
	// bank that is not going to accept the transfer.
	maxAttempts = 3
)

// StateQueue satisfies settle.Payouts.
//
// There is no broker here: an order's status column is the queue. Deposit
// detection moves it to payout_queued and Worker picks it up from there, which
// means a restart loses nothing — there is no in-flight delivery to drop and no
// watch to restart.
type StateQueue struct{}

func (StateQueue) Enqueue(string) error { return nil }

// Worker disburses NGN for orders whose deposits are confirmed.
type Worker struct {
	DB       *gorm.DB
	Provider Provider
	Log      *slog.Logger
	Interval time.Duration
	Batch    int

	// Bus ends a wait as soon as a deposit is confirmed, so a payout starts in
	// the same second rather than at whatever point this loop's interval next
	// came round.
	Bus *wake.Bus
}

// Run processes payouts until the context is cancelled.
func (w *Worker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	wake.Loop{
		Name:     "payout worker",
		Topic:    wake.Payouts,
		Interval: interval,
		Bus:      w.Bus,
		Log:      w.Log,
		Work:     w.sweep,
	}.Run(ctx)
}

func (w *Worker) sweep(ctx context.Context) (found bool) {
	defer func() {
		if rec := recover(); rec != nil {
			w.Log.Error("payout sweep panicked", "panic", rec)
		}
	}()

	batch := w.Batch
	if batch <= 0 {
		batch = defaultBatch
	}

	var orders []store.Order
	if err := w.DB.
		Where("status = ?", store.StatePayoutQueued).
		Order("deposited_at ASC").
		Limit(batch).
		Find(&orders).Error; err != nil {
		w.Log.Error("payout sweep query failed", "error", err)
		return false
	}

	for i := range orders {
		if ctx.Err() != nil {
			return len(orders) > 0
		}
		w.pay(&orders[i])
	}
	return len(orders) > 0
}

// pay disburses one order.
func (w *Worker) pay(order *store.Order) {
	// Claim before paying. Without this, two workers — or one worker and a
	// retry of itself — could each submit a disbursement for the same order.
	won, err := store.ClaimStatus(w.DB, order.ID,
		store.StatePayoutQueued, store.StatePayoutProcessing)
	if err != nil {
		w.Log.Error("could not claim order for payout", "order", order.ID, "error", err)
		return
	}
	if !won {
		return
	}

	result, err := w.Provider.Pay(Request{
		Reference:    order.ID,
		AmountNGN:    order.AmountNGN,
		Currency:     order.Currency,
		BankCode:     order.BankCode,
		BankAccount:  order.BankAccount,
		AccountName:  order.AccountName,
		BankName:     order.BankName,
		SourceTxHash: order.DepositTxHash,
	})
	if err != nil {
		// The request may have arrived and only the response been lost, so this
		// must never be read as "no payout happened". Ask the provider what it
		// knows before deciding anything.
		w.Log.Warn("payout submission failed, checking provider state",
			"order", order.ID, "error", err)
		if known, statusErr := w.Provider.Status(order.ID); statusErr == nil && known.Status != StatusQueued {
			w.settle(order, known)
			return
		}
		w.retryOrRefund(order, err.Error())
		return
	}

	w.settle(order, result)
}

// settle applies a provider result to an order.
func (w *Worker) settle(order *store.Order, result Result) {
	switch result.Status {
	case StatusPaid:
		w.markDisbursed(order, result)
	case StatusFailed:
		w.retryOrRefund(order, result.FailureReason)
	default:
		// Still in flight. Hand the order back so the next sweep re-checks it
		// rather than holding it in payout_processing indefinitely.
		if err := store.ReleaseStatus(w.DB, order.ID,
			store.StatePayoutProcessing, store.StatePayoutQueued); err != nil {
			w.Log.Error("could not release in-flight payout", "order", order.ID, "error", err)
		}
	}
}

// markDisbursed records a successful payout, once.
//
// ClaimFinancialOutcome is the guard that stops an order both paying out and
// refunding. If it is already claimed as refunded, this payout arrived after
// the refund path won — the money has gone somewhere it should not have, and
// that needs a human, not a state transition.
func (w *Worker) markDisbursed(order *store.Order, result Result) {
	claimed, err := store.ClaimFinancialOutcome(w.DB, order.ID, "disbursed")
	if err != nil {
		w.Log.Error("could not claim financial outcome", "order", order.ID, "error", err)
		return
	}
	if !claimed {
		w.Log.Error("payout succeeded on an order already resolved; needs review",
			"order", order.ID, "payoutRef", result.Reference)
		return
	}

	now := time.Now().UTC()
	if _, err := store.ClaimStatusWith(w.DB, order.ID,
		store.StatePayoutProcessing, store.StateDisbursed, map[string]any{
			"payout_reference": result.Reference,
			"payout_provider":  result.Provider,
			"paid_out_at":      &now,
		}); err != nil {
		w.Log.Error("could not mark order disbursed", "order", order.ID, "error", err)
		return
	}

	w.Log.Info("ngn disbursed",
		"order", order.ID, "amountNgn", order.AmountNGN, "payoutRef", result.Reference)

	// The merchant is paid; the USDC can now move to treasury. Sweeping only
	// after disbursement means a failed payout still has the deposit sitting in
	// the order's own account, where the refund path can return it.
	if _, err := store.ClaimStatus(w.DB, order.ID,
		store.StateDisbursed, store.StateSweepQueued); err != nil {
		w.Log.Error("could not queue treasury sweep", "order", order.ID, "error", err)
	}
}

// retryOrRefund gives a failing payout a few more chances before returning the
// payer's money.
func (w *Worker) retryOrRefund(order *store.Order, reason string) {
	attempts := order.PayoutAttempts + 1
	if err := w.DB.Model(&store.Order{}).
		Where("id = ?", order.ID).
		Update("payout_attempts", attempts).Error; err != nil {
		w.Log.Error("could not record payout attempt", "order", order.ID, "error", err)
	}

	if attempts < maxAttempts {
		w.Log.Warn("payout failed, will retry",
			"order", order.ID, "attempt", attempts, "reason", reason)
		if err := store.ReleaseStatusBecause(w.DB, order.ID,
			store.StatePayoutProcessing, store.StatePayoutQueued, reason); err != nil {
			w.Log.Error("could not requeue payout", "order", order.ID, "error", err)
		}
		return
	}

	w.Log.Error("payout failed permanently, refunding",
		"order", order.ID, "attempts", attempts, "reason", reason)
	if _, err := store.ClaimStatusBecause(w.DB, order.ID,
		store.StatePayoutProcessing, store.StateRefundQueued, reason); err != nil {
		w.Log.Error("could not queue refund", "order", order.ID, "error", err)
	}
}
