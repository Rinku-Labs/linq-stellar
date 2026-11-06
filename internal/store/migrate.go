package store

import "gorm.io/gorm"

// Migrate creates or updates the schema this service owns.
//
// The settlement orders themselves, the transitions each one made, the Horizon
// stream cursors that make reconnects safe, the pool of deposit accounts
// provisioned ahead of the orders that will use them, and the integrators, keys
// and rate counters behind the public API, and every payout attempt with the
// reference it sent. Nothing else — this service does not
// own merchant records or bank details beyond what it needs to settle one
// payment.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&Order{}, &StatusEvent{}, &StreamCursor{}, &PoolAccount{},
		&Integrator{}, &APIKey{}, &CredentialEvent{}, &RateCounter{},
		&PayoutAttempt{},
	); err != nil {
		return err
	}
	if err := seedLegacyIntegrator(db); err != nil {
		return err
	}
	// Idempotency is per integrator now. The composite index is already in
	// place by this point, so the orders table is never without a guard. Rows
	// cannot collide on it: the old index had them unique on the key alone.
	if err := db.Exec(`DROP INDEX IF EXISTS idx_stellar_orders_idempotency_key`).Error; err != nil {
		return err
	}
	if err := protectCredentialEvents(db); err != nil {
		return err
	}
	return createWorkerIndexes(db)
}

// protectCredentialEvents makes the rotation and revocation proof append-only:
// a row anyone could quietly edit would prove nothing. Postgres only; the tests'
// SQLite has no such trigger.
//
// The advisory lock is here because server and worker both migrate at startup,
// and two concurrent CREATE OR REPLACE FUNCTION calls can fail outright.
func protectCredentialEvents(db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	return db.Exec(`
		DO $migrate$
		BEGIN
			PERFORM pg_advisory_xact_lock(7283945);
			CREATE OR REPLACE FUNCTION credential_events_append_only() RETURNS trigger AS $fn$
			BEGIN
				RAISE EXCEPTION 'credential_events is append-only';
			END
			$fn$ LANGUAGE plpgsql;
			IF NOT EXISTS (SELECT 1 FROM pg_trigger
			                WHERE tgname = 'credential_events_append_only'
			                  AND tgrelid = 'credential_events'::regclass) THEN
				CREATE TRIGGER credential_events_append_only
					BEFORE UPDATE OR DELETE ON credential_events
					FOR EACH ROW EXECUTE FUNCTION credential_events_append_only();
			END IF;
		END
		$migrate$`).Error
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

		// Pool claim: the next ready account, oldest first. This one runs
		// inside order creation, where it is the difference between handing a
		// payer an address in milliseconds and provisioning one while they
		// wait.
		`CREATE INDEX IF NOT EXISTS idx_pool_ready
		   ON stellar_deposit_pool (id)
		   WHERE claimed_at IS NULL AND ready = true`,

		// One `expired` row per key, however many callers notice it.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_credential_events_expired
		   ON credential_events (key_id)
		   WHERE action = 'expired'`,

		// Pruning: counters are keyed by key first, so the delete by age needs
		// its own path.
		`CREATE INDEX IF NOT EXISTS idx_rate_counters_window
		   ON api_rate_counters (window_start)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
