package store

import "gorm.io/gorm"

// Migrate creates or updates the schema this service owns.
//
// Three tables: the settlement orders themselves, the transitions each one
// made, and the Horizon stream cursors that make reconnects safe. Nothing else
// — this service does not own merchant records or bank details beyond what it
// needs to settle one payment.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&Order{}, &StatusEvent{}, &StreamCursor{}); err != nil {
		return err
	}
	return createWorkerIndexes(db)
}

// createWorkerIndexes adds one composite index per worker query.
//
// Every settlement loop filters on status and orders by a timestamp. A plain
// index on status alone still leaves Postgres sorting the matches on every
// pass, and these run continuously — so the sort, not the lookup, is what
// shows up on the bill.
//
// Each index is written to match a specific query. Changing a worker's ORDER BY
// without changing its index here quietly gives the sort back.
func createWorkerIndexes(db *gorm.DB) error {
	statements := []string{
		// Scanner and stream supervisor: awaiting deposit, deadline first.
		`CREATE INDEX IF NOT EXISTS idx_orders_status_deadline
		   ON stellar_orders (status, deposit_deadline)`,

		// Payout worker: queued payouts, oldest deposit first.
		`CREATE INDEX IF NOT EXISTS idx_orders_status_deposited
		   ON stellar_orders (status, deposited_at)`,

		// Chain worker: sweep and refund queues, least recently touched first.
		`CREATE INDEX IF NOT EXISTS idx_orders_status_updated
		   ON stellar_orders (status, updated_at)`,

		// Reclaim. Partial, because it only ever looks at expired orders that
		// have not been reclaimed — and once the service has been running a
		// while that is a vanishing fraction of the table.
		`CREATE INDEX IF NOT EXISTS idx_orders_reclaim_pending
		   ON stellar_orders (updated_at)
		   WHERE status = 'expired' AND reserves_reclaimed = false`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
