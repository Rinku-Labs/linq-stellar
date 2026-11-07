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
	// unknownOutcomeDelay is how long an order whose payout outcome could not
	// be learned waits before the provider is asked again.
	unknownOutcomeDelay = 30 * time.Second
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

	// NewReferenceOnRetry (PAYOUT_NEW_REFERENCE_ON_RETRY) gives each attempt
	// after a confirmed, retry-safe failure its own reference, so the retry
	// really pays. A failure the provider does not confirm as retry-safe goes
	// to StateFailed for a person to check.
	//
	// Off, every attempt reuses the order ID. The provider is idempotent on it,
	// so a retry reads back the first failure and the order is refunded after
	// maxAttempts, as before this switch existed.
	NewReferenceOnRetry bool
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
		Where("payout_after IS NULL OR payout_after <= ?", time.Now().UTC()).
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
//
// Each try is a store.PayoutAttempt, recorded before its request is sent. A
// pending attempt is resumed with the same reference; a new attempt, with a new
// reference, starts only after the last one failed and the provider confirmed
// it.
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

	last, err := store.LatestPayoutAttempt(w.DB, order.ID)
	if err != nil {
		w.Log.Error("could not load payout attempts", "order", order.ID, "error", err)
		w.wait(order, "")
		return
	}

	var attempt store.PayoutAttempt
	switch {
	case last != nil && last.Outcome == store.PayoutPending:
		// The last request's outcome was never learned. Resend the same
		// reference: the provider returns that payout rather than starting
		// another.
		attempt = *last
	case last != nil && last.Outcome == store.PayoutPaid:
		// Paid, but the process stopped before the order moved on. Ask again
		// for the provider's reference, which markDisbursed records.
		w.confirm(order, *last)
		return
	case last != nil && w.NewReferenceOnRetry && !last.RetrySafe:
		// Failed while the switch was off, and not confirmed retry-safe. A new
		// reference now could pay twice, so a person decides, as for any
		// such failure with the switch on.
		w.failed(order, *last, last.Reason, false)
		return
	default:
		n := 1
		if last != nil {
			n = last.Number + 1
		}
		if w.induced(order, n) {
			ref := store.PayoutReference(order.ID, n, w.NewReferenceOnRetry)
			induced, err := store.RecordInducedFailure(w.DB, order.ID, n, ref)
			if err != nil {
				w.Log.Error("could not record induced payout failure", "order", order.ID, "error", err)
				w.wait(order, "")
				return
			}
			w.Log.Warn("payout failure induced, provider not called",
				"order", order.ID, "attempt", n, "kind", order.InducedFailure)
			w.failed(order, induced, store.InducedReason, true)
			return
		}
		attempt, err = store.StartPayoutAttempt(w.DB, order.ID, n,
			store.PayoutReference(order.ID, n, w.NewReferenceOnRetry))
		if err != nil {
			w.Log.Error("could not record payout attempt", "order", order.ID, "attempt", n, "error", err)
			w.wait(order, "")
			return
		}
	}

	result, err := w.Provider.Pay(Request{
		Reference:    attempt.Reference,
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
		// knows before deciding anything, and if it cannot say, wait.
		w.Log.Warn("payout submission failed, checking provider state",
			"order", order.ID, "reference", attempt.Reference, "error", err)
		w.confirm(order, attempt)
		return
	}

	if result.Status == StatusFailed {
		// Only the status lookup's "failed" is trusted, as it is the provider's
		// settled record of the reference.
		w.confirm(order, attempt)
		return
	}
	w.settle(order, attempt, result)
}

// induced reports whether attempt n of a test order should fail on purpose.
func (w *Worker) induced(order *store.Order, n int) bool {
	// Read fresh: the order was listed before it was claimed, and a failure
	// may have been induced in between.
	kind, err := store.InducedFailureOf(w.DB, order.ID)
	if err != nil {
		w.Log.Error("could not read induced failure", "order", order.ID, "error", err)
		return false
	}
	order.InducedFailure = kind
	switch kind {
	case store.InducedTerminal:
		return true
	case store.InducedTransient:
		return n == 1
	}
	return false
}

// confirm asks the provider for an attempt's settled outcome and acts on it.
func (w *Worker) confirm(order *store.Order, attempt store.PayoutAttempt) {
	known, err := w.Provider.Status(attempt.Reference)
	if err != nil {
		w.Log.Warn("payout outcome unknown, will check again",
			"order", order.ID, "reference", attempt.Reference, "error", err)
		w.waitFor(order, "payout outcome unknown; checking again", unknownOutcomeDelay)
		return
	}
	w.settle(order, attempt, known)
}

// settle applies a provider result to an order. A StatusFailed result must
// come from the provider's status lookup; see pay.
func (w *Worker) settle(order *store.Order, attempt store.PayoutAttempt, result Result) {
	switch result.Status {
	case StatusPaid:
		if err := store.ResolvePayoutAttempt(w.DB, attempt.ID, store.PayoutPaid, false, ""); err != nil {
			w.Log.Error("could not record paid payout attempt", "order", order.ID, "error", err)
		}
		w.markDisbursed(order, result)
	case StatusFailed:
		if err := store.ResolvePayoutAttempt(w.DB, attempt.ID, store.PayoutFailed,
			result.RetrySafe, result.FailureReason); err != nil {
			// Leave the attempt pending, so the next pass asks again rather
			// than starting a new reference over a failure it never recorded.
			w.Log.Error("could not record failed payout attempt", "order", order.ID, "error", err)
			w.wait(order, "")
			return
		}
		w.failed(order, attempt, result.FailureReason, result.RetrySafe)
	default:
		// Still in flight. Hand the order back so the next sweep re-checks it
		// rather than holding it in payout_processing indefinitely.
		w.wait(order, "")
	}
}

// wait hands an order back to payout_queued with its attempt unchanged, so the
// next sweep asks about the same reference again. It never counts as a failed
// attempt and never leads to a refund: the payout may still be in flight.
func (w *Worker) wait(order *store.Order, reason string) {
	w.waitFor(order, reason, 0)
}

// waitFor is wait, holding the order back from the next sweeps for delay.
func (w *Worker) waitFor(order *store.Order, reason string, delay time.Duration) {
	var after *time.Time
	if delay > 0 {
		at := time.Now().UTC().Add(delay)
		after = &at
	}
	// Set before the release, while this worker still holds the claim, so no
	// sweep can pick the order up between the two writes.
	if err := w.DB.Model(&store.Order{}).
		Where("id = ? AND status = ?", order.ID, store.StatePayoutProcessing).
		Update("payout_after", after).Error; err != nil {
		w.Log.Error("could not set payout backoff", "order", order.ID, "error", err)
	}
	if err := store.ReleaseStatusBecause(w.DB, order.ID,
		store.StatePayoutProcessing, store.StatePayoutQueued, reason); err != nil {
		w.Log.Error("could not release in-flight payout", "order", order.ID, "error", err)
	}
}

// failed acts on a confirmed failure: retry, refund, or hand to a person.
func (w *Worker) failed(order *store.Order, attempt store.PayoutAttempt, reason string, retrySafe bool) {
	if err := w.DB.Model(&store.Order{}).
		Where("id = ?", order.ID).
		Update("payout_attempts", attempt.Number).Error; err != nil {
		w.Log.Error("could not record payout attempt count", "order", order.ID, "error", err)
	}

	// A new reference after a failure that may have reached the bank could pay
	// the merchant a second time, and a refund on top of a payout that went
	// through pays the payer twice. Neither is safe, so a person decides.
	if w.NewReferenceOnRetry && !retrySafe {
		w.Log.Error("payout failed without confirmation that no money moved; needs review",
			"order", order.ID, "reference", attempt.Reference, "reason", reason)
		if _, err := store.ClaimStatusBecause(w.DB, order.ID,
			store.StatePayoutProcessing, store.StateFailed,
			"payout failed and the provider did not confirm that no money moved: "+reason); err != nil {
			w.Log.Error("could not mark payout for review", "order", order.ID, "error", err)
		}
		return
	}

	if attempt.Number < maxAttempts {
		w.Log.Warn("payout failed, will retry",
			"order", order.ID, "attempt", attempt.Number, "reference", attempt.Reference, "reason", reason)
		if err := store.ReleaseStatusBecause(w.DB, order.ID,
			store.StatePayoutProcessing, store.StatePayoutQueued, reason); err != nil {
			w.Log.Error("could not requeue payout", "order", order.ID, "error", err)
		}
		return
	}

	w.Log.Error("payout failed permanently, refunding",
		"order", order.ID, "attempts", attempt.Number, "reason", reason)
	if _, err := store.ClaimStatusBecause(w.DB, order.ID,
		store.StatePayoutProcessing, store.StateRefundQueued, reason); err != nil {
		w.Log.Error("could not queue refund", "order", order.ID, "error", err)
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
