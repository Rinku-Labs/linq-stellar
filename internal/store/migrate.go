package store

import "gorm.io/gorm"

// Migrate creates or updates the schema this service owns.
//
// The settlement orders themselves, the transitions each one made, the Horizon
// stream cursors that make reconnects safe, the pool of deposit accounts
// provisioned ahead of the orders that will use them, the integrators, keys
// and rate counters behind the public API, every payout attempt with the
// reference it sent, and the reconciliation record of those books checked
// against the ledger. Nothing else — this service does not
// own merchant records or bank details beyond what it needs to settle one
// payment.
//
// Server and worker both migrate at startup, and two CREATE TABLEs for the same
// table collide in Postgres's catalog even with IF NOT EXISTS. So the whole of it
// runs in one transaction behind an advisory lock: the second instance waits, then
// finds nothing left to do. DDL is transactional in Postgres, so a failure leaves
// no half-applied schema either. A statement that cannot run in a transaction,
// such as CREATE INDEX CONCURRENTLY, does not belong in here.
func Migrate(db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return migrate(db)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", migrationLock).Error; err != nil {
			return err
		}
		return migrate(tx)
	})
}

// migrationLock is an arbitrary number shared by everything that migrates.
const migrationLock = 7283945

func migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&Order{}, &StatusEvent{}, &StreamCursor{}, &PoolAccount{},
		&Integrator{}, &APIKey{}, &CredentialEvent{}, &RateCounter{}, &WebhookDelivery{},
		&PayoutAttempt{},
		&ReconciliationCycle{}, &ReconciliationItem{},
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
	if err := moveReclaimHashes(db); err != nil {
		return err
	}
	return createWorkerIndexes(db)
}

// moveReclaimHashes moves each reclaim transaction out of sweep_tx_hash, where
// the reclaim worker used to record it, into reclaim_tx_hash. An expired order
// is never swept: the sweep worker only takes orders that were paid out. So a
// hash in sweep_tx_hash on an expired order is always a reclaim, and
// reconciliation reported it as a sweep that moved no USDC, every cycle.
//
// It changes nothing once every such row is moved, so it is safe on every boot.
func moveReclaimHashes(db *gorm.DB) error {
	return db.Exec(`UPDATE stellar_orders
		SET reclaim_tx_hash = sweep_tx_hash, sweep_tx_hash = ''
		WHERE status = ? AND reserves_reclaimed = ? AND sweep_tx_hash <> '' AND reclaim_tx_hash = ''`,
		StateExpired, true).Error
}

// protectCredentialEvents makes the rotation and revocation proof append-only:
// a row anyone could quietly edit would prove nothing. Postgres only; the tests'
// SQLite has no such trigger.
func protectCredentialEvents(db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	return db.Exec(`
		DO $migrate$
		BEGIN
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

		// One rejection row per key, action and minute. See RecordRejection.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_credential_events_rejected
		   ON credential_events (key_id, action, rejected_minute)
		   WHERE rejected_minute IS NOT NULL`,

		// One `expired` row per key, however many callers notice it.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_credential_events_expired
		   ON credential_events (key_id)
		   WHERE action = 'expired'`,

		// The notifier's every pass. See owedEvents.
		`CREATE INDEX IF NOT EXISTS idx_status_events_owed
		   ON stellar_order_status_events (at, id)
		   WHERE ` + owedEvents,

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
