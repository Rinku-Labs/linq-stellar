package store

import (
	"time"

	"gorm.io/gorm"
)

// Status history.
//
// The order row carries the state an order is in now, and a handful of
// timestamps for the moments worth naming — deposited, paid out, swept. What
// it cannot answer is "how long did that take", because a column that is
// overwritten on every move remembers only the last one.
//
// That question is the whole of the reliability story. "Payouts retry" is a
// claim about code; "this payout failed at 12:04:18 and the refund was queued
// at 12:04:33" is a fact about a specific order, checkable by anyone holding
// its id. This table is what makes the second kind of statement possible, and
// it is written from the same three functions that move an order, so a
// transition cannot happen without being recorded.

// StatusEvent is one recorded transition.
type StatusEvent struct {
	ID      uint   `gorm:"primaryKey" json:"-"`
	OrderID string `gorm:"index;size:255" json:"-"`
	// From is empty for the first event of an order's life.
	From string `json:"from"`
	To   string `json:"to"`
	// Reason explains a move that was not the happy path — a payout provider's
	// rejection, say. Empty when the transition speaks for itself.
	Reason string `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

func (StatusEvent) TableName() string { return "stellar_order_status_events" }

// recordTransition stores one move.
//
// Best-effort by design, and deliberately not returned to the caller: every
// call site has just moved money or claimed the right to. Failing a settled
// transition because its audit row would not write would turn a bookkeeping
// problem into a financial one. A missing row shows up as a gap in the
// history, which is the safer way to find out.
func recordTransition(db *gorm.DB, orderID, from, to, reason string) {
	_ = db.Create(&StatusEvent{
		OrderID: orderID,
		From:    from,
		To:      to,
		Reason:  reason,
		At:      time.Now().UTC(),
	}).Error
}

// StatusHistory returns an order's transitions, oldest first.
func StatusHistory(db *gorm.DB, orderID string) ([]StatusEvent, error) {
	var events []StatusEvent
	err := db.Where("order_id = ?", orderID).
		Order("at ASC, id ASC").
		Find(&events).Error
	return events, err
}

// ElapsedBetween reports how long an order took to get from one state to
// another, using the first occurrence of each.
//
// Returns false when either state is absent, so a caller cannot accidentally
// report a zero duration for something that never happened.
func ElapsedBetween(events []StatusEvent, from, to string) (time.Duration, bool) {
	var start, end time.Time
	for _, e := range events {
		if start.IsZero() && e.To == from {
			start = e.At
		}
		if !start.IsZero() && e.To == to && !e.At.Before(start) {
			end = e.At
			break
		}
	}
	if start.IsZero() || end.IsZero() {
		return 0, false
	}
	return end.Sub(start), true
}
