package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/apikey"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LegacyIntegratorID owns every order that came through ORDERS_API_KEY: all of
// them before /v1 existed, and everything Linq-B2B creates on the legacy routes
// since. It is the column default on stellar_orders, which is what backfills
// the existing rows and what covers an old server still writing mid-deploy.
const LegacyIntegratorID = "int_linq_b2b"

// RotationOverlap is how long a rotated key keeps working, so an integrator can
// roll the new key out without a moment when neither works.
const RotationOverlap = 24 * time.Hour

const (
	IntegratorActive    = "active"
	IntegratorSuspended = "suspended"
)

// The actions recorded in credential_events.
const (
	CredentialIssued          = "issued"
	CredentialRotated         = "rotated"
	CredentialRevoked         = "revoked"
	CredentialExpired         = "expired"
	CredentialRejectedRevoked = "rejected_revoked"
	CredentialRejectedExpired = "rejected_expired"
)

// ActorAdmin and ActorSystem name who did something when it was not the
// integrator's own key. See ActorKey.
const (
	ActorAdmin  = "admin"
	ActorSystem = "system"
)

// ActorKey names a key acting on itself.
func ActorKey(keyID string) string { return "key:" + keyID }

var (
	ErrIntegratorNotFound = errors.New("store: integrator not found")
	ErrKeyNotFound        = errors.New("store: api key not found")
	// ErrKeyNotRotatable means the key is revoked, expired, or already replaced.
	// The last case matters most: if a key inside its overlap could rotate
	// again, whoever holds a leaked key could mint a fresh one and outlive the
	// rotation meant to shut them out.
	ErrKeyNotRotatable = errors.New("store: api key cannot be rotated")
)

// Integrator is a business calling the public API. It is not Order.BusinessID,
// which is the merchant being paid.
type Integrator struct {
	ID     string `gorm:"primaryKey" json:"id"`
	Name   string `gorm:"not null" json:"name"`
	Status string `gorm:"not null;default:active;check:chk_integrators_status,status IN ('active','suspended')" json:"status"`
	// WebhookURL and WebhookSecretEncrypted are unused until signed webhooks.
	WebhookURL             string `json:"-"`
	WebhookSecretEncrypted string `json:"-"`
	// AllowFailureInjection is for Linq's own test integrators only.
	AllowFailureInjection bool `gorm:"not null;default:false" json:"-"`
	// RateLimitPerMinute overrides DefaultRateLimitPerMinute for each of this
	// integrator's keys. Nil means the default.
	RateLimitPerMinute *int `gorm:"check:chk_integrators_rate_limit,rate_limit_per_minute > 0" json:"rateLimitPerMinute"`
	CreatedAt          time.Time `json:"createdAt"`
}

func (Integrator) TableName() string { return "integrators" }

// APIKey is one credential. The key itself is never stored: only the prefix it
// is looked up by and a hash to check it against.
type APIKey struct {
	ID           string     `gorm:"primaryKey"`
	IntegratorID string     `gorm:"not null;index"`
	Integrator   Integrator `gorm:"foreignKey:IntegratorID"`
	KeyPrefix    string     `gorm:"not null;uniqueIndex"`
	SecretHash   string     `gorm:"not null"`
	CreatedAt    time.Time
	ExpiresAt    *time.Time
	RevokedAt    *time.Time
	LastUsedAt   *time.Time
	// ReplacedBy is the key this one was rotated to. Empty means it has not
	// been rotated; see ErrKeyNotRotatable.
	ReplacedBy string
}

func (APIKey) TableName() string { return "api_keys" }

// Expired reports whether a key's expiry has passed.
func (k APIKey) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && !k.ExpiresAt.After(now)
}

// CredentialEvent is one row of the rotation and revocation proof. Append only.
type CredentialEvent struct {
	ID           uint64    `gorm:"primaryKey" json:"id"`
	IntegratorID string    `gorm:"not null;index" json:"integratorId"`
	KeyID        string    `gorm:"index" json:"keyId,omitempty"`
	Action       string    `gorm:"not null;check:chk_credential_events_action,action IN ('issued','rotated','revoked','expired','rejected_revoked','rejected_expired')" json:"action"`
	Actor        string    `gorm:"not null" json:"actor"`
	Note         string    `json:"note,omitempty"`
	At           time.Time `gorm:"not null" json:"at"`
}

func (CredentialEvent) TableName() string { return "credential_events" }

// IssuedKey is a key at the only moment its plaintext exists.
type IssuedKey struct {
	APIKey
	Full string
}

// CreateIntegrator adds a caller of the public API. limit may be nil.
func CreateIntegrator(db *gorm.DB, name string, limit *int) (Integrator, error) {
	id, err := apikey.NewID("int")
	if err != nil {
		return Integrator{}, err
	}
	integrator := Integrator{
		ID:                 id,
		Name:               name,
		Status:             IntegratorActive,
		RateLimitPerMinute: limit,
	}
	return integrator, db.Create(&integrator).Error
}

// SetRateLimit sets or, with nil, clears an integrator's override.
func SetRateLimit(db *gorm.DB, integratorID string, limit *int) error {
	result := db.Model(&Integrator{}).Where("id = ?", integratorID).
		Update("rate_limit_per_minute", limit)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrIntegratorNotFound
	}
	return nil
}

// IssueKey creates a key for an integrator and records that it did.
func IssueKey(db *gorm.DB, integratorID, mode, actor string, now time.Time) (IssuedKey, error) {
	var issued IssuedKey
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&Integrator{}, "id = ?", integratorID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrIntegratorNotFound
			}
			return err
		}
		var err error
		issued, err = issueKey(tx, integratorID, mode, actor, now)
		return err
	})
	return issued, err
}

func issueKey(tx *gorm.DB, integratorID, mode, actor string, now time.Time) (IssuedKey, error) {
	id, err := apikey.NewID("key")
	if err != nil {
		return IssuedKey{}, err
	}
	full, prefix, err := apikey.New(mode)
	if err != nil {
		return IssuedKey{}, err
	}
	key := APIKey{
		ID:           id,
		IntegratorID: integratorID,
		KeyPrefix:    prefix,
		SecretHash:   apikey.Hash(full),
		CreatedAt:    now,
	}
	if err := tx.Omit("Integrator").Create(&key).Error; err != nil {
		return IssuedKey{}, err
	}
	if err := RecordCredentialEvent(tx, integratorID, id, CredentialIssued, actor, "", now); err != nil {
		return IssuedKey{}, err
	}
	return IssuedKey{APIKey: key, Full: full}, nil
}

// RotateKey issues a replacement and gives the old key until now+RotationOverlap.
// An expiry already sooner than that is left alone: rotating never buys a key
// more time. It returns the new key and the old one as it now stands.
func RotateKey(db *gorm.DB, keyID, mode, actor string, now time.Time) (IssuedKey, APIKey, error) {
	var issued IssuedKey
	var old APIKey
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&old, "id = ?", keyID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrKeyNotFound
			}
			return err
		}
		if old.RevokedAt != nil || old.ReplacedBy != "" || old.Expired(now) {
			return ErrKeyNotRotatable
		}

		var err error
		issued, err = issueKey(tx, old.IntegratorID, mode, actor, now)
		if err != nil {
			return err
		}

		until := now.Add(RotationOverlap)
		if old.ExpiresAt != nil && old.ExpiresAt.Before(until) {
			until = *old.ExpiresAt
		}
		// A compare-and-swap, for the same reason ClaimStatus is one: of two
		// concurrent rotations exactly one may win. The loser's transaction
		// rolls back, taking its new key with it.
		result := tx.Model(&APIKey{}).
			Where("id = ? AND revoked_at IS NULL AND (replaced_by IS NULL OR replaced_by = '')", keyID).
			Updates(map[string]any{"expires_at": until, "replaced_by": issued.ID})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrKeyNotRotatable
		}
		old.ExpiresAt, old.ReplacedBy = &until, issued.ID

		note := fmt.Sprintf("replaced by %s; old key valid until %s",
			issued.ID, until.UTC().Format(time.RFC3339))
		return RecordCredentialEvent(tx, old.IntegratorID, keyID, CredentialRotated, actor, note, now)
	})
	if err != nil {
		return IssuedKey{}, APIKey{}, err
	}
	return issued, old, nil
}

// RevokeKey stops a key immediately. Revoking a revoked key changes nothing and
// is not an error, so it records nothing either.
func RevokeKey(db *gorm.DB, keyID, actor string, now time.Time) (APIKey, error) {
	var key APIKey
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&key, "id = ?", keyID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrKeyNotFound
			}
			return err
		}
		result := tx.Model(&APIKey{}).Where("id = ? AND revoked_at IS NULL", keyID).
			Update("revoked_at", now)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		key.RevokedAt = &now
		return RecordCredentialEvent(tx, key.IntegratorID, keyID, CredentialRevoked, actor, "", now)
	})
	if err != nil {
		return APIKey{}, err
	}
	return key, nil
}

// FindKey loads a key and what authentication needs of its integrator, in one
// query because this runs on every authenticated request. Named columns, so
// the webhook secret stays out of the request path.
func FindKey(db *gorm.DB, prefix string) (APIKey, error) {
	var row struct {
		APIKey
		Status             string
		RateLimitPerMinute *int
	}
	result := db.Raw(`
		SELECT k.id, k.integrator_id, k.key_prefix, k.secret_hash,
		       k.expires_at, k.revoked_at, k.last_used_at, k.replaced_by,
		       i.status, i.rate_limit_per_minute
		  FROM api_keys k
		  JOIN integrators i ON i.id = k.integrator_id
		 WHERE k.key_prefix = ?`, prefix).Scan(&row)
	if result.Error != nil {
		return APIKey{}, result.Error
	}
	if result.RowsAffected == 0 {
		return APIKey{}, gorm.ErrRecordNotFound
	}
	key := row.APIKey
	key.Integrator = Integrator{
		ID:                 key.IntegratorID,
		Status:             row.Status,
		RateLimitPerMinute: row.RateLimitPerMinute,
	}
	return key, nil
}

// usageResolution is how stale last_used_at may be. Writing it on every request
// would add an update of the same row to the hot path, to record a fact nobody
// needs to the second.
const usageResolution = time.Minute

// TouchKey records that a key was used.
func TouchKey(db *gorm.DB, key APIKey, now time.Time) error {
	if key.LastUsedAt != nil && now.Sub(*key.LastUsedAt) < usageResolution {
		return nil
	}
	// Exec rather than Update: one statement needs no BEGIN and COMMIT around it.
	return db.Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, now, key.ID).Error
}

// RecordCredentialEvent appends to the proof.
func RecordCredentialEvent(db *gorm.DB, integratorID, keyID, action, actor, note string, at time.Time) error {
	return db.Create(&CredentialEvent{
		IntegratorID: integratorID,
		KeyID:        keyID,
		Action:       action,
		Actor:        actor,
		Note:         note,
		At:           at,
	}).Error
}

// RecordExpiredKeys writes the `expired` row for every key whose expiry has
// passed and which does not have one yet. Nothing happens at the moment a key
// expires, so this runs from the worker and again whenever an expired key is
// refused. The event carries the expiry time rather than the time of the sweep,
// and idx_credential_events_expired makes it safe for two callers to race.
func RecordExpiredKeys(db *gorm.DB, now time.Time) (int64, error) {
	result := db.Exec(`
		INSERT INTO credential_events (integrator_id, key_id, action, actor, note, at)
		SELECT k.integrator_id, k.id, ?, ?, '', k.expires_at
		  FROM api_keys k
		 WHERE k.expires_at <= ?
		   AND (k.revoked_at IS NULL OR k.revoked_at > k.expires_at)
		   AND NOT EXISTS (SELECT 1 FROM credential_events e
		                    WHERE e.key_id = k.id AND e.action = ?)
		ON CONFLICT DO NOTHING`,
		CredentialExpired, ActorSystem, now, CredentialExpired)
	return result.RowsAffected, result.Error
}

// CredentialEventFilter narrows ListCredentialEvents. Empty fields match all.
type CredentialEventFilter struct {
	IntegratorID string
	KeyID        string
	Limit        int
}

// ListCredentialEvents returns the newest events first.
func ListCredentialEvents(db *gorm.DB, f CredentialEventFilter) ([]CredentialEvent, error) {
	query := db.Order("id DESC").Limit(f.Limit)
	if f.IntegratorID != "" {
		query = query.Where("integrator_id = ?", f.IntegratorID)
	}
	if f.KeyID != "" {
		query = query.Where("key_id = ?", f.KeyID)
	}
	events := []CredentialEvent{}
	return events, query.Find(&events).Error
}

// seedLegacyIntegrator makes sure the owner of the legacy orders exists.
func seedLegacyIntegrator(db *gorm.DB) error {
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&Integrator{
		ID:     LegacyIntegratorID,
		Name:   "linq-b2b",
		Status: IntegratorActive,
	}).Error
}
