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

// Event kinds. One table rather than two, because the question being asked is
// "what happened to this order, in order" — and an answer that makes you merge
// two lists by timestamp yourself is not one timeline, it is two.
const (
	// KindTransition is a move between lifecycle states.
	KindTransition = "transition"
	// KindNotification is an attempt to tell the Linq backend about one.
	KindNotification = "notification"
)

// StatusEvent is one recorded thing that happened to an order.
type StatusEvent struct {
	ID      uint   `gorm:"primaryKey" json:"-"`
	OrderID string `gorm:"index;size:255" json:"-"`
	// Kind distinguishes a state change from an attempt to report one.
	// Defaults to KindTransition for rows written before this existed.
	Kind string `gorm:"index;default:transition" json:"kind"`
	// From is empty for the first event of an order's life, and for
	// notifications, which describe a status rather than a move between two.
	From string `json:"from,omitempty"`
	To   string `json:"to"`
	// Reason explains a move that was not the happy path — a payout provider's
	// rejection, say — or why a delivery failed. Empty when the event speaks
	// for itself.
	Reason string    `json:"reason,omitempty"`
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
		Kind:    KindTransition,
		From:    from,
		To:      to,
		Reason:  reason,
		At:      time.Now().UTC(),
	}).Error
}

// RecordNotification stores one attempt to report a status downstream.
//
// Written to the same table as the transitions so an order's history is a
// single ordered account of what happened and who was told. Keeping delivery
// attempts only in the container log would mean the two halves of every
// incident — the order moved, nobody was told — live in different systems, and
// only one of them can be handed to someone else.
//
// Best-effort, for the same reason as recordTransition.
func RecordNotification(db *gorm.DB, orderID, status, reason string) {
	_ = db.Create(&StatusEvent{
		OrderID: orderID,
		Kind:    KindNotification,
		To:      status,
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
		// Transitions only. A notification row names the status it reported,
		// so counting one as an arrival would measure to the moment a state
		// was announced rather than the moment it was reached.
		if e.Kind == KindNotification {
			continue
		}
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
