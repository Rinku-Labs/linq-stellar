package settle

import (
	"context"
	"log/slog"
	"time"

	"github.com/uselinq/linq-stellar/internal/stellar"
	"github.com/uselinq/linq-stellar/internal/store"
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
}

// Run works the chain queues until the context is cancelled.
func (w *ChainWorker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = defaultChainInterval
	}
	w.Log.Info("chain worker started", "interval", interval, "treasury", w.Treasury)

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		w.each(ctx, store.StateSweepQueued, w.sweep)
		w.each(ctx, store.StateRefundQueued, w.refund)
		w.each(ctx, store.StateExpired, w.reclaim)
		select {
		case <-ctx.Done():
			w.Log.Info("chain worker stopped")
			return
		case <-t.C:
		}
	}
}

// each runs fn over a batch of orders in one state.
func (w *ChainWorker) each(ctx context.Context, status string, fn func(*store.Order)) {
	defer func() {
		if rec := recover(); rec != nil {
			w.Log.Error("chain worker panicked", "status", status, "panic", rec)
		}
	}()

	batch := w.Batch
	if batch <= 0 {
		batch = defaultChainBatch
	}

	var orders []store.Order
	if err := w.DB.
		Where("status = ?", status).
		Order("updated_at ASC").
		Limit(batch).
		Find(&orders).Error; err != nil {
		w.Log.Error("chain worker query failed", "status", status, "error", err)
		return
	}
	for i := range orders {
		if ctx.Err() != nil {
			return
		}
		fn(&orders[i])
	}
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
		return // already closed
	}

	hash, err := w.Chain.Reclaim(order.EncryptedSeed)
	if err != nil {
		w.Log.Warn("could not reclaim deposit account", "order", order.ID, "error", err)
		return
	}
	if hash == "" {
		return // nothing left to close
	}

	if err := w.DB.Model(&store.Order{}).
		Where("id = ?", order.ID).
		Updates(map[string]any{"sweep_tx_hash": hash, "updated_at": time.Now().UTC()}).Error; err != nil {
		w.Log.Error("reclaimed but could not record it", "order", order.ID, "tx", hash, "error", err)
		return
	}
	w.Log.Info("reclaimed sponsor reserves", "order", order.ID, "tx", hash)
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
