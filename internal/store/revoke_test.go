package store

import (
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
)

// A revocation racing a rotation must never leave a working key behind. Whichever
// commits first, the other has to see it: rotation refuses a revoked key, and
// revocation reads the successor only after it holds the row.
func TestARotationRacingARevocationLeavesNoLiveKey(t *testing.T) {
	if os.Getenv("LINQ_TEST_PG_DSN") == "" {
		t.Skip("needs LINQ_TEST_PG_DSN")
	}
	open := postgresSchema(t)
	first, second := open(), open()
	integrator, err := CreateIntegrator(first, "race", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UTC()
	for round := 0; round < 40; round++ {
		key, err := IssueKey(first, integrator.ID, "live", ActorAdmin, now)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		var rotated IssuedKey
		go func() {
			defer wg.Done()
			rotated, _, _ = RotateKey(first, key.ID, "live", ActorKey(key.ID), now)
		}()
		go func() {
			defer wg.Done()
			if _, _, err := RevokeKey(second, key.ID, ActorAdmin, now); err != nil {
				t.Errorf("revoke: %v", err)
			}
		}()
		wg.Wait()

		if rotated.ID == "" {
			continue
		}
		var successor APIKey
		if err := first.First(&successor, "id = ?", rotated.ID).Error; err != nil {
			t.Fatalf("load: %v", err)
		}
		if successor.RevokedAt == nil {
			t.Fatalf("round %d: the rotated key outlived the revocation", round)
		}
	}
}

func TestAnIssuedKeyCannotBelongToLinqB2B(t *testing.T) {
	db := poolDB(t)
	if _, err := IssueKey(db, LegacyIntegratorID, "live", ActorAdmin, time.Now().UTC()); err != ErrLegacyIntegrator {
		t.Errorf("err = %v, want ErrLegacyIntegrator", err)
	}
}

// The append-only trigger and the partial unique index are Postgres's, so this
// has to hold there, not only on the tests' SQLite.
func TestARejectionIsRecordedOncePerMinute(t *testing.T) {
	engines(t, func(t *testing.T, open func() *gorm.DB) {
		db := open()
		key := APIKey{ID: "key_r", IntegratorID: "int_r"}
		now := time.Date(2026, 11, 8, 12, 0, 15, 0, time.UTC)
		for _, at := range []time.Time{now, now.Add(10 * time.Second), now.Add(time.Minute)} {
			if err := RecordRejection(db, key, CredentialRejectedRevoked, at); err != nil {
				t.Fatalf("record: %v", err)
			}
		}
		if err := RecordRejection(db, key, CredentialRejectedExpired, now); err != nil {
			t.Fatalf("record: %v", err)
		}
		var revoked, expired int64
		db.Model(&CredentialEvent{}).Where("action = ?", CredentialRejectedRevoked).Count(&revoked)
		db.Model(&CredentialEvent{}).Where("action = ?", CredentialRejectedExpired).Count(&expired)
		if revoked != 2 || expired != 1 {
			t.Errorf("revoked rows = %d, expired rows = %d, want 2 and 1", revoked, expired)
		}
	})
}
