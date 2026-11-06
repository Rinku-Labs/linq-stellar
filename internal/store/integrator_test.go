package store

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// engines runs a test on SQLite always, and on Postgres too when
// LINQ_TEST_PG_DSN points at a local database. The schema upgrade and the
// concurrency claims are about Postgres behaviour, so they are only proven there.
func engines(t *testing.T, run func(t *testing.T, open func() *gorm.DB)) {
	t.Run("sqlite", func(t *testing.T) {
		db := poolDB(t)
		run(t, func() *gorm.DB { return db })
	})
	if os.Getenv("LINQ_TEST_PG_DSN") != "" {
		t.Run("postgres", func(t *testing.T) { run(t, postgresSchema(t)) })
	}
}

// postgresSchema gives a test its own schema, migrated, and returns a way to
// open independent connection pools onto it: two pools stand in for two server
// instances.
//
// Only a local server is accepted. This writes and drops things, and a
// misdirected DSN must fail here rather than reach a real database.
func postgresSchema(t *testing.T) func() *gorm.DB {
	t.Helper()
	open := emptySchema(t)
	if err := Migrate(open()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return open
}

// emptySchema is postgresSchema before any migration has run.
func emptySchema(t *testing.T) func() *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LINQ_TEST_PG_DSN")
	if !isLocalDSN(dsn) {
		t.Fatalf("LINQ_TEST_PG_DSN must point at localhost or 127.0.0.1")
	}

	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	open := func(dsn string) *gorm.DB {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		t.Cleanup(func() {
			if sql, err := db.DB(); err == nil {
				_ = sql.Close()
			}
		})
		return db
	}

	admin := open(dsn)
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE") })

	scoped := dsn + " search_path=" + schema
	return func() *gorm.DB { return open(scoped) }
}

func isLocalDSN(dsn string) bool {
	if parsed, err := url.Parse(dsn); err == nil && parsed.Host != "" {
		return parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1"
	}
	return strings.Contains(dsn, "host=localhost") || strings.Contains(dsn, "host=127.0.0.1")
}

func TestAnUpgradeBackfillsOrdersAndMovesIdempotencyPerIntegrator(t *testing.T) {
	engines(t, func(t *testing.T, open func() *gorm.DB) {
		db := open()

		// Put the database back to how main left it, holding an order.
		for _, statement := range []string{
			`DROP INDEX idx_orders_integrator_idempotency`,
			`ALTER TABLE stellar_orders DROP COLUMN integrator_id`,
			`CREATE UNIQUE INDEX idx_stellar_orders_idempotency_key ON stellar_orders (idempotency_key)`,
			`DELETE FROM integrators`,
			`INSERT INTO stellar_orders (id, idempotency_key, status) VALUES ('old-order', 'old-key', 'awaiting_deposit')`,
		} {
			if err := db.Exec(statement).Error; err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}

		for i := 0; i < 2; i++ { // twice: every deploy runs this again
			if err := Migrate(db); err != nil {
				t.Fatalf("migrate pass %d: %v", i+1, err)
			}
		}

		var order Order
		if err := db.First(&order, "id = ?", "old-order").Error; err != nil {
			t.Fatalf("load order: %v", err)
		}
		if order.IntegratorID != LegacyIntegratorID {
			t.Errorf("integrator = %q, want the backfill", order.IntegratorID)
		}
		var legacy Integrator
		if err := db.First(&legacy, "id = ?", LegacyIntegratorID).Error; err != nil {
			t.Fatalf("the legacy integrator was not seeded: %v", err)
		}
		if legacy.Name != "linq-b2b" {
			t.Errorf("name = %q", legacy.Name)
		}

		if db.Migrator().HasIndex(&Order{}, "idx_stellar_orders_idempotency_key") {
			t.Error("the global idempotency index survived")
		}
		if err := db.Create(&Order{ID: "other", IntegratorID: "int_other", IdempotencyKey: "old-key"}).Error; err != nil {
			t.Errorf("another integrator could not use the same key: %v", err)
		}
		if err := db.Create(&Order{ID: "dupe", IdempotencyKey: "old-key"}).Error; err == nil {
			t.Error("one integrator used the same key twice")
		}
		// An old server mid-deploy names no integrator; the default must catch it.
		if err := db.Exec(`INSERT INTO stellar_orders (id, idempotency_key, status)
			VALUES ('from-old-server', 'k2', 'awaiting_deposit')`).Error; err != nil {
			t.Fatalf("insert: %v", err)
		}
		var midDeploy Order
		db.First(&midDeploy, "id = ?", "from-old-server")
		if midDeploy.IntegratorID != LegacyIntegratorID {
			t.Errorf("mid-deploy order integrator = %q", midDeploy.IntegratorID)
		}
	})
}

func TestAPIKeysReferenceARealIntegrator(t *testing.T) {
	if os.Getenv("LINQ_TEST_PG_DSN") == "" {
		t.Skip("foreign keys are only enforced on Postgres here")
	}
	db := postgresSchema(t)()
	err := db.Omit("Integrator").Create(&APIKey{ID: "k", IntegratorID: "int_nobody", KeyPrefix: "p", SecretHash: "h"}).Error
	if err == nil {
		t.Error("a key was stored for an integrator that does not exist")
	}
}

func TestRecordExpiredKeys(t *testing.T) {
	engines(t, func(t *testing.T, open func() *gorm.DB) {
		db := open()
		now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		past, future := now.Add(-time.Hour), now.Add(time.Hour)
		revokedFirst := past.Add(-time.Minute)

		for _, key := range []APIKey{
			{ID: "lapsed", ExpiresAt: &past},
			{ID: "still-valid", ExpiresAt: &future},
			{ID: "no-expiry"},
			{ID: "revoked-first", ExpiresAt: &past, RevokedAt: &revokedFirst},
		} {
			key.IntegratorID, key.KeyPrefix, key.SecretHash = LegacyIntegratorID, key.ID, "hash"
			if err := db.Omit("Integrator").Create(&key).Error; err != nil {
				t.Fatalf("seed: %v", err)
			}
		}

		for pass, want := range []int64{1, 0} {
			written, err := RecordExpiredKeys(db, now)
			if err != nil {
				t.Fatalf("pass %d: %v", pass, err)
			}
			if written != want {
				t.Errorf("pass %d wrote %d, want %d", pass, written, want)
			}
		}

		var event CredentialEvent
		if err := db.First(&event, "action = ?", CredentialExpired).Error; err != nil {
			t.Fatalf("load event: %v", err)
		}
		if event.KeyID != "lapsed" || !event.At.Equal(past) || event.Actor != ActorSystem {
			t.Errorf("event = %+v, want lapsed at the expiry time, by system", event)
		}
	})
}

// Two workers noticing the same expiry must leave one row.
func TestExpiryIsRecordedOnceUnderConcurrency(t *testing.T) {
	if os.Getenv("LINQ_TEST_PG_DSN") == "" {
		t.Skip("needs LINQ_TEST_PG_DSN")
	}
	open := postgresSchema(t)
	db := open()
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	if err := db.Omit("Integrator").Create(&APIKey{
		ID: "lapsed", IntegratorID: LegacyIntegratorID, KeyPrefix: "p", SecretHash: "h", ExpiresAt: &past,
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		instance := open()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := RecordExpiredKeys(instance, now); err != nil {
				t.Errorf("record: %v", err)
			}
		}()
	}
	wg.Wait()

	var rows int64
	db.Model(&CredentialEvent{}).Where("action = ?", CredentialExpired).Count(&rows)
	if rows != 1 {
		t.Errorf("%d expired rows, want 1", rows)
	}
}

func TestOnlyOneConcurrentRotationWins(t *testing.T) {
	if os.Getenv("LINQ_TEST_PG_DSN") == "" {
		t.Skip("needs LINQ_TEST_PG_DSN")
	}
	open := postgresSchema(t)
	db := open()
	now := time.Now().UTC()
	key, err := IssueKey(db, LegacyIntegratorID, "live", ActorAdmin, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		instance := open()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := RotateKey(instance, key.ID, "live", ActorAdmin, now); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()

	if wins.Load() != 1 {
		t.Errorf("%d rotations succeeded, want 1", wins.Load())
	}
	var keys int64
	db.Model(&APIKey{}).Count(&keys)
	if keys != 2 {
		t.Errorf("%d keys exist, want 2: a losing rotation left its key behind", keys)
	}
}

func TestRotationNeverExtendsAnExpiry(t *testing.T) {
	db := poolDB(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	key, err := IssueKey(db, LegacyIntegratorID, "live", ActorAdmin, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	soon := now.Add(time.Hour)
	if err := db.Model(&APIKey{}).Where("id = ?", key.ID).Update("expires_at", soon).Error; err != nil {
		t.Fatalf("set expiry: %v", err)
	}

	_, old, err := RotateKey(db, key.ID, "live", ActorAdmin, now)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !old.ExpiresAt.Equal(soon) {
		t.Errorf("expiry moved to %v, want %v", old.ExpiresAt, soon)
	}
}

func TestLastUsedIsRecordedCoarsely(t *testing.T) {
	db := poolDB(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	issued, err := IssueKey(db, LegacyIntegratorID, "live", ActorAdmin, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	load := func() APIKey {
		var key APIKey
		if err := db.First(&key, "id = ?", issued.ID).Error; err != nil {
			t.Fatalf("load: %v", err)
		}
		return key
	}
	if err := TouchKey(db, load(), now); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if got := load().LastUsedAt; got == nil || !got.Equal(now) {
		t.Fatalf("last used = %v", got)
	}
	if err := TouchKey(db, load(), now.Add(30*time.Second)); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if got := load().LastUsedAt; !got.Equal(now) {
		t.Errorf("rewritten inside the resolution: %v", got)
	}
	later := now.Add(2 * time.Minute)
	if err := TouchKey(db, load(), later); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if got := load().LastUsedAt; !got.Equal(later) {
		t.Errorf("not rewritten after the resolution: %v", got)
	}
}

func TestRateCountersAreKeyedByKeyAndMinute(t *testing.T) {
	engines(t, func(t *testing.T, open func() *gorm.DB) {
		db := open()
		minute := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

		hit := func(key string, window time.Time) int {
			t.Helper()
			count, err := HitRateCounter(db, key, window)
			if err != nil {
				t.Fatalf("hit: %v", err)
			}
			return count
		}
		for want := 1; want <= 3; want++ {
			if got := hit("a", minute); got != want {
				t.Errorf("count = %d, want %d", got, want)
			}
		}
		if got := hit("b", minute); got != 1 {
			t.Errorf("another key shared the count: %d", got)
		}
		if got := hit("a", minute.Add(time.Minute)); got != 1 {
			t.Errorf("the next minute shared the count: %d", got)
		}
	})
}

// The whole point of keeping this in Postgres: instances hitting one counter at
// the same moment each get their own total, with none lost.
func TestConcurrentHitsFromTwoInstancesAreAllCounted(t *testing.T) {
	if os.Getenv("LINQ_TEST_PG_DSN") == "" {
		t.Skip("needs LINQ_TEST_PG_DSN")
	}
	open := postgresSchema(t)
	instances := []*gorm.DB{open(), open()}
	for _, instance := range instances {
		sql, err := instance.DB()
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		sql.SetMaxOpenConns(10)
	}
	window := time.Now().UTC().Truncate(time.Minute)

	const hits, limit = 300, 120
	var allowed atomic.Int32
	seen := make([]atomic.Int32, hits+1)

	var wg sync.WaitGroup
	for i := 0; i < hits; i++ {
		wg.Add(1)
		go func(db *gorm.DB) {
			defer wg.Done()
			count, err := HitRateCounter(db, "busy", window)
			if err != nil {
				t.Errorf("hit: %v", err)
				return
			}
			seen[count].Add(1)
			if count <= limit {
				allowed.Add(1)
			}
		}(instances[i%2])
	}
	wg.Wait()

	if allowed.Load() != limit {
		t.Errorf("%d requests allowed, want exactly %d", allowed.Load(), limit)
	}
	for count := 1; count <= hits; count++ {
		if seen[count].Load() != 1 {
			t.Errorf("total %d was returned %d times, want once", count, seen[count].Load())
		}
	}
}

func TestPruneDeletesCountersOlderThanAnHour(t *testing.T) {
	engines(t, func(t *testing.T, open func() *gorm.DB) {
		db := open()
		now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		for name, age := range map[string]time.Duration{
			"current":  0,
			"recent":   59 * time.Minute,
			"at-limit": time.Hour,
			"stale":    61 * time.Minute,
			"very-old": 24 * time.Hour,
		} {
			if _, err := HitRateCounter(db, name, now.Add(-age)); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}

		pruned, err := PruneRateCounters(db, now)
		if err != nil {
			t.Fatalf("prune: %v", err)
		}
		if pruned != 2 {
			t.Errorf("pruned %d, want 2", pruned)
		}
		var keys []string
		db.Model(&RateCounter{}).Order("key_id").Pluck("key_id", &keys)
		if strings.Join(keys, ",") != "at-limit,current,recent" {
			t.Errorf("kept %v", keys)
		}
	})
}

// Server and worker migrate at startup, so a deploy runs this concurrently on a
// schema that has work to do.
func TestConcurrentMigrationsDoNotCollide(t *testing.T) {
	if os.Getenv("LINQ_TEST_PG_DSN") == "" {
		t.Skip("needs LINQ_TEST_PG_DSN")
	}
	for round := 0; round < 3; round++ {
		open := emptySchema(t)
		var wg sync.WaitGroup
		errs := make(chan error, 6)
		for i := 0; i < 6; i++ {
			instance := open()
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- Migrate(instance)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("round %d: %v", round, err)
			}
		}
	}
}

// credential_events is what the submission shows as proof, so the application
// must not be able to rewrite it, by bug or otherwise.
func TestCredentialEventsAreAppendOnly(t *testing.T) {
	if os.Getenv("LINQ_TEST_PG_DSN") == "" {
		t.Skip("needs LINQ_TEST_PG_DSN")
	}
	db := postgresSchema(t)()
	now := time.Now().UTC()
	if err := RecordCredentialEvent(db, LegacyIntegratorID, "k", CredentialIssued, ActorAdmin, "", now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Exec(`UPDATE credential_events SET actor = 'someone else'`).Error; err == nil {
		t.Error("an event was rewritten")
	}
	if err := db.Exec(`DELETE FROM credential_events`).Error; err == nil {
		t.Error("an event was deleted")
	}
	// A second migration must leave the guard in place and not fail on it.
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate again: %v", err)
	}
	if err := db.Exec(`DELETE FROM credential_events`).Error; err == nil {
		t.Error("an event was deleted after a second migration")
	}
}

// The columns the rules depend on hold only what the code knows how to read.
func TestStatusAndActionAreConstrained(t *testing.T) {
	if os.Getenv("LINQ_TEST_PG_DSN") == "" {
		t.Skip("needs LINQ_TEST_PG_DSN")
	}
	db := postgresSchema(t)()
	if err := db.Exec(`UPDATE integrators SET status = 'bogus'`).Error; err == nil {
		t.Error("an integrator took an unknown status")
	}
	if err := db.Exec(`UPDATE integrators SET rate_limit_per_minute = 0`).Error; err == nil {
		t.Error("an integrator took a zero rate limit")
	}
	if err := RecordCredentialEvent(db, LegacyIntegratorID, "k", "bogus", ActorAdmin, "", time.Now()); err == nil {
		t.Error("an unknown credential action was recorded")
	}
}

func TestOldDeliveriesArePrunedAndRecentOnesKept(t *testing.T) {
	engines(t, func(t *testing.T, open func() *gorm.DB) {
		db := open()
		now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		for _, at := range []time.Time{now.Add(-91 * 24 * time.Hour), now.Add(-89 * 24 * time.Hour)} {
			row := WebhookDelivery{OrderID: "o", IntegratorID: "i", Status: "disbursed", Attempt: 1, URLHost: "h", At: at}
			if err := RecordWebhookDelivery(db, row); err != nil {
				t.Fatalf("record: %v", err)
			}
		}
		deleted, err := PruneWebhookDeliveries(db, now)
		if err != nil || deleted != 1 {
			t.Fatalf("deleted = %d, %v", deleted, err)
		}
		var left int64
		db.Model(&WebhookDelivery{}).Count(&left)
		if left != 1 {
			t.Errorf("%d rows left, want 1", left)
		}
	})
}
