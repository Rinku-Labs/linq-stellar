package store

import (
	"time"

	"gorm.io/gorm"
)

// Proof queries for the D4 page on the reviewer dashboard. They select named
// columns only, so no bank account, account name or bank name is ever read.

// ProofColumns are the order columns the proof page may show.
var ProofColumns = []string{
	"id", "business_id", "created_at", "status", "financial_outcome", "induced_failure",
	"deposit_tx_hash", "sweep_tx_hash", "refund_tx_hash",
	"amount_usdc", "amount_ngn", "payout_reference", "payout_provider",
	"deposited_at", "paid_out_at", "refunded_at",
}

// ProofScope is whose orders count as the D4 proof: one integrator's orders,
// optionally one merchant business's, created at or after Since.
type ProofScope struct {
	IntegratorID string
	BusinessID   uint
	Since        time.Time
}

// ProofOrders returns the scope's orders that received a deposit, oldest first.
func ProofOrders(db *gorm.DB, s ProofScope) ([]Order, error) {
	q := db.Model(&Order{}).Select(ProofColumns).
		Where("integrator_id = ? AND created_at >= ? AND deposit_tx_hash <> ''", s.IntegratorID, s.Since)
	if s.BusinessID != 0 {
		q = q.Where("business_id = ?", s.BusinessID)
	}
	var orders []Order
	err := q.Order("created_at ASC, id ASC").Find(&orders).Error
	return orders, err
}

// InducedOrders returns an integrator's orders whose payout failed on purpose,
// oldest first.
func InducedOrders(db *gorm.DB, integratorID string) ([]Order, error) {
	var orders []Order
	err := db.Model(&Order{}).Select(ProofColumns).
		Where("integrator_id = ? AND induced_failure <> ''", integratorID).
		Order("created_at ASC, id ASC").Find(&orders).Error
	return orders, err
}

// NotificationTimes says how quickly the status changes of these orders reached
// their integrator: how many were delivered, how many took longer than limit,
// and the slowest.
func NotificationTimes(db *gorm.DB, orderIDs []string, limit time.Duration) (delivered, over int, slowest time.Duration, err error) {
	if len(orderIDs) == 0 {
		return 0, 0, 0, nil
	}
	var events []StatusEvent
	if err = db.Select("at", "notified_at").
		Where("order_id IN ? AND notified_at IS NOT NULL", orderIDs).Find(&events).Error; err != nil {
		return
	}
	for _, e := range events {
		d := e.NotifiedAt.Sub(e.At)
		delivered++
		if d > limit {
			over++
		}
		if d > slowest {
			slowest = d
		}
	}
	return
}
