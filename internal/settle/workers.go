package settle

import (
	"context"
	"log/slog"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/Rinku-Labs/linq-stellar/internal/wake"
	"gorm.io/gorm"
)

const (
	defaultChainInterval = 10 * time.Second
	defaultChainBatch    = 50
	maxChainAttempts     = 3
)

// ChainWorker performs the on-chain half of settlement: sweeping paid orders to
// treasury, refunding ones whose payout failed, and reclaiming the reserves of
// orders that were never funded.
//
// All three are grouped because they share a shape — claim, submit one Stellar
// transaction, record the hash — and because all three end with the sponsor
// getting its reserves back.
type ChainWorker struct {
	DB       *gorm.DB
	Chain    *stellar.Client
	Treasury string
	Log      *slog.Logger
	Interval time.Duration
	Batch    int

	// Bus ends a wait when a sweep or a refund is queued, so a payer whose
	// payout failed gets their USDC back in the same minute rather than
	// whenever this loop next came round.
	Bus *wake.Bus
}

// Run works the chain queues until the context is cancelled.
func (w *ChainWorker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = defaultChainInterval
	}

	wake.Loop{
		Name:     "chain worker",
		Topic:    wake.Chain,
		Interval: interval,
		Bus:      w.Bus,
		Log:      w.Log,
		Fields:   []any{"treasury", w.Treasury},
		Work:     w.pass,
	}.Run(ctx)
}

// pass works all three chain queues once.
func (w *ChainWorker) pass(ctx context.Context) bool {
	worked := w.each(ctx, store.StateSweepQueued, nil, w.sweep)
	worked = w.each(ctx, store.StateRefundQueued, nil, w.refund) || worked
	// Expired is a terminal state, so this query would never drain without
	// the reserves_reclaimed filter — every expired order would be
	// re-selected on every pass for the life of the service.
	worked = w.each(ctx, store.StateExpired, func(q *gorm.DB) *gorm.DB {
		return q.Where("reserves_reclaimed = ?", false)
	}, w.reclaim) || worked
	return worked
}

// each runs fn over a batch of orders in one state, and reports whether it
// found any. narrow may add further conditions to the query.
func (w *ChainWorker) each(ctx context.Context, status string, narrow func(*gorm.DB) *gorm.DB, fn func(*store.Order)) (found bool) {
	defer func() {
		if rec := recover(); rec != nil {
			w.Log.Error("chain worker panicked", "status", status, "panic", rec)
		}
	}()

	batch := w.Batch
	if batch <= 0 {
		batch = defaultChainBatch
	}

	q := w.DB.Where("status = ?", status)
	if narrow != nil {
		q = narrow(q)
	}

	var orders []store.Order
	if err := q.
		Order("updated_at ASC").
		Limit(batch).
		Find(&orders).Error; err != nil {
		w.Log.Error("chain worker query failed", "status", status, "error", err)
		return false
	}
	for i := range orders {
		if ctx.Err() != nil {
			return len(orders) > 0
		}
		fn(&orders[i])
	}
	return len(orders) > 0
}

// sweep moves a disbursed order's USDC to treasury and closes its account.
func (w *ChainWorker) sweep(order *store.Order) {
	won, err := store.ClaimStatus(w.DB, order.ID, store.StateSweepQueued, store.StateSweepProcessing)
	if err != nil || !won {
		return
	}

	hash, err := w.Chain.Sweep(order.EncryptedSeed, w.Treasury, order.AmountUSDC)
	if err != nil {
		w.retry(order, store.StateSweepProcessing, store.StateSweepQueued,
			"sweep_attempts", order.SweepAttempts, "treasury sweep failed", err)
		return
	}

	now := time.Now().UTC()
	if _, err := store.ClaimStatusWith(w.DB, order.ID,
		store.StateSweepProcessing, store.StateSettledInTreasury, map[string]any{
			"sweep_tx_hash": hash,
			"swept_at":      &now,
		}); err != nil {
		// The USDC has moved and the account is closed. Only the record is
		// behind, and it must be reconciled by hand rather than by retrying,
		// because retrying would fail against an account that no longer exists.
		w.Log.Error("swept on-chain but could not record it; needs reconciliation",
			"order", order.ID, "tx", hash, "error", err)
		return
	}
	w.Log.Info("settled in treasury", "order", order.ID, "usdc", order.AmountUSDC, "tx", hash)
}

// refund returns the deposit to the payer after a failed payout.
func (w *ChainWorker) refund(order *store.Order) {
	destination := order.RefundAddress
	if destination == "" {
		// The payer's own account is the only remaining destination. It is
		// known to hold a USDC trustline, because it just sent USDC.
		destination = order.DepositFrom
	}
	if destination == "" {
		w.Log.Error("no refund destination; funds held for manual handling",
			"order", order.ID, "address", order.DepositAddress)
		return
	}

	won, err := store.ClaimStatus(w.DB, order.ID, store.StateRefundQueued, store.StateRefundProcessing)
	if err != nil || !won {
		return
	}

	// Claim the outcome before moving money. If the payout path has already
	// claimed "disbursed", the merchant has been paid and refunding would send
	// the money twice.
	claimed, err := store.ClaimFinancialOutcome(w.DB, order.ID, "refunded")
	if err != nil {
		w.Log.Error("could not claim financial outcome", "order", order.ID, "error", err)
		return
	}
	if !claimed {
		w.Log.Error("refund requested on an order already resolved; needs review", "order", order.ID)
		return
	}

	hash, err := w.Chain.Sweep(order.EncryptedSeed, destination, order.AmountUSDC)
	if err != nil {
		w.retry(order, store.StateRefundProcessing, store.StateRefundQueued,
			"refund_attempts", order.RefundAttempts, "refund failed", err)
		return
	}

	now := time.Now().UTC()
	if _, err := store.ClaimStatusWith(w.DB, order.ID,
		store.StateRefundProcessing, store.StateRefunded, map[string]any{
			"refund_tx_hash": hash,
			"refunded_at":    &now,
		}); err != nil {
		w.Log.Error("refunded on-chain but could not record it; needs reconciliation",
			"order", order.ID, "tx", hash, "error", err)
		return
	}
	w.Log.Info("refunded to payer", "order", order.ID, "destination", destination, "tx", hash)
}

// reclaim closes the account of an order that never received a deposit.
//
// Without this every expired order strands roughly 1.5 XLM of sponsor reserves
// permanently. At volume that is a steady drain on the account this service
// depends on to provision anything at all.
func (w *ChainWorker) reclaim(order *store.Order) {
	if order.SweepTxHash != "" || order.RefundTxHash != "" {
		// Already closed by a sweep or refund; just stop looking at it.
		w.markReclaimed(order, nil)
		return
	}

	hash, err := w.Chain.Reclaim(order.EncryptedSeed)
	if err != nil {
		// Leave it unmarked so the next pass retries — a Horizon failure is not
		// evidence there is nothing to reclaim.
		w.Log.Warn("could not reclaim deposit account", "order", order.ID, "error", err)
		return
	}

	// An empty hash means there was nothing left to close, which is a finished
	// outcome rather than a reason to look again. Marking it is what stops this
	// order being re-checked against Horizon on every pass for the life of the
	// service.
	fields := map[string]any{}
	if hash != "" {
		fields["sweep_tx_hash"] = hash
	}
	w.markReclaimed(order, fields)

	if hash != "" {
		w.Log.Info("reclaimed sponsor reserves", "order", order.ID, "tx", hash)
	}
}

// markReclaimed takes an expired order out of the reclaim queue for good.
func (w *ChainWorker) markReclaimed(order *store.Order, extra map[string]any) {
	fields := map[string]any{
		"reserves_reclaimed": true,
		"updated_at":         time.Now().UTC(),
	}
	for k, v := range extra {
		fields[k] = v
	}
	if err := w.DB.Model(&store.Order{}).
		Where("id = ?", order.ID).
		Updates(fields).Error; err != nil {
		w.Log.Error("could not mark order reclaimed", "order", order.ID, "error", err)
	}
}

// retry backs an order out for another attempt, or gives up and flags it.
func (w *ChainWorker) retry(order *store.Order, from, to, column string, attempts int, what string, cause error) {
	next := attempts + 1
	if err := w.DB.Model(&store.Order{}).
		Where("id = ?", order.ID).
		Update(column, next).Error; err != nil {
		w.Log.Error("could not record attempt", "order", order.ID, "error", err)
	}

	if next < maxChainAttempts {
		w.Log.Warn(what+", will retry", "order", order.ID, "attempt", next, "error", cause)
		if err := store.ReleaseStatus(w.DB, order.ID, from, to); err != nil {
			w.Log.Error("could not requeue", "order", order.ID, "error", err)
		}
		return
	}

	// Out of attempts. The money is still in the order's own account, which is
	// the safest place for it to sit until someone looks.
	w.Log.Error(what+" permanently; funds remain in the deposit account",
		"order", order.ID, "address", order.DepositAddress, "attempts", next, "error", cause)
	if _, err := store.ClaimStatus(w.DB, order.ID, from, store.StateFailed); err != nil {
		w.Log.Error("could not mark order failed", "order", order.ID, "error", err)
	}
}
