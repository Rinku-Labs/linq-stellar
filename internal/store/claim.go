package store

import (
	"time"

	"gorm.io/gorm"
)

// Claiming.
//
// More than one thing detects a deposit: the Horizon stream sees it in about a
// second, and the polling sweep catches whatever the stream dropped. That
// redundancy is deliberate — a stream that silently misses an event means a
// payer who paid and got nothing — but it means two goroutines can arrive at
// the same order at the same moment.
//
// Every one of them must enter through ClaimStatus. A detector that publishes
// downstream without claiming first will eventually pay an order twice, and
// there is no amount of care elsewhere that prevents it. One door to the money,
// no exceptions.

// ClaimStatus atomically moves an order from one state to another.
//
// It returns true only for the caller that actually performed the move. A false
// return is the normal outcome when something else got there first: nothing has
// gone wrong, and the caller should stop rather than retry.
//
// The condition and the write are one statement so there is no window between
// checking the state and changing it.
func ClaimStatus(db *gorm.DB, orderID, from, to string) (bool, error) {
	if !CanTransition(from, to) {
		return false, nil
	}
	res := db.Model(&Order{}).
		Where("id = ? AND status = ?", orderID, from).
		Updates(map[string]any{
			"status":     to,
			"updated_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ClaimStatusWith moves an order and writes extra columns in the same statement.
//
// Use it when the fields being recorded are only meaningful if the claim
// succeeds — a deposit's transaction hash, say. Writing those separately after
// a successful claim leaves a window where the order has moved but the evidence
// of why has not been stored yet.
func ClaimStatusWith(db *gorm.DB, orderID, from, to string, fields map[string]any) (bool, error) {
	if !CanTransition(from, to) {
		return false, nil
	}
	updates := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		updates[k] = v
	}
	updates["status"] = to
	updates["updated_at"] = time.Now().UTC()

	res := db.Model(&Order{}).
		Where("id = ? AND status = ?", orderID, from).
		Updates(updates)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ReleaseStatus hands a claim back after downstream work failed to start.
//
// Claiming an order and then failing to queue the job behind it would strand it
// in a state nothing is working on. Releasing puts it back where the detectors
// can find it again. It is conditional on the order still being in the state
// this caller left it in, so a release cannot stamp on someone else's progress.
func ReleaseStatus(db *gorm.DB, orderID, from, to string) error {
	return db.Model(&Order{}).
		Where("id = ? AND status = ?", orderID, from).
		Updates(map[string]any{
			"status":     to,
			"updated_at": time.Now().UTC(),
		}).Error
}

// ClaimFinancialOutcome records, exactly once, whether an order was disbursed
// or refunded.
//
// This is the last guard against an order doing both. The payout path and the
// refund path can be running concurrently — a payout that is slow to report
// success looks identical to one that failed — so whichever claims the outcome
// first decides it, and the loser must not move money.
//
// Returns true only for the caller that set it.
func ClaimFinancialOutcome(db *gorm.DB, orderID, outcome string) (bool, error) {
	res := db.Model(&Order{}).
		Where("id = ? AND (financial_outcome = '' OR financial_outcome IS NULL)", orderID).
		Updates(map[string]any{
			"financial_outcome": outcome,
			"updated_at":        time.Now().UTC(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}
