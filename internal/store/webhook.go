package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// owedEvents selects the transitions still to be reported. It is both the WHERE
// clause and the predicate of idx_status_events_owed, word for word, so the
// planner can use that index. Without it this reads every row with a null
// notified_at, which is most of the table: notification rows and transitions
// nobody is told about keep that column null forever.
//
// The states are store.Notifiable's, spelled out because an index predicate
// cannot take parameters.
const owedEvents = `kind = 'transition' AND notified_at IS NULL AND notify_skipped_at IS NULL
	AND "to" IN ('deposit_detected', 'disbursed', 'settled_in_treasury',
	             'refund_queued', 'refunded', 'expired', 'failed')`

const dueNow = `(notify_after IS NULL OR notify_after <= ?)`

// webhookDeliveryRetention is how long the delivery log is kept: long enough to
// answer a dispute about a missed notification.
const webhookDeliveryRetention = 90 * 24 * time.Hour

// pruneBatch keeps each delete short, so pruning never holds a long lock.
const pruneBatch = 5000

// WebhookDelivery is one attempt to report a status change. It holds the host
// only: a webhook path can carry the integrator's own token.
type WebhookDelivery struct {
	ID           uint64 `gorm:"primaryKey"`
	OrderID      string `gorm:"not null;index;size:255"`
	IntegratorID string `gorm:"not null;index"`
	// Status is the order status being reported.
	Status  string `gorm:"not null"`
	Attempt int    `gorm:"not null;check:chk_webhook_deliveries_attempt,attempt > 0"`
	URLHost string `gorm:"column:url_host;not null"`
	// ResponseCode is zero when the receiver never answered.
	ResponseCode int
	LatencyMS    int64 `gorm:"column:latency_ms;not null"`
	// Error is empty on success.
	Error string
	At    time.Time `gorm:"not null;index"`
}

func (WebhookDelivery) TableName() string { return "webhook_deliveries" }

// RecordWebhookDelivery stores one attempt.
func RecordWebhookDelivery(db *gorm.DB, delivery WebhookDelivery) error {
	return db.Create(&delivery).Error
}

// PruneWebhookDeliveries deletes rows past their retention, a batch at a time.
func PruneWebhookDeliveries(db *gorm.DB, now time.Time) (int64, error) {
	var total int64
	cutoff := now.Add(-webhookDeliveryRetention)
	for {
		result := db.Exec(`DELETE FROM webhook_deliveries WHERE id IN
			(SELECT id FROM webhook_deliveries WHERE at < ? LIMIT ?)`, cutoff, pruneBatch)
		if result.Error != nil {
			return total, result.Error
		}
		total += result.RowsAffected
		if result.RowsAffected < pruneBatch {
			return total, nil
		}
	}
}

// SetWebhookURL moves an integrator's events to a new URL, keeping its secret.
func SetWebhookURL(db *gorm.DB, integratorID, url string) error {
	result := db.Model(&Integrator{}).Where("id = ?", integratorID).Update("webhook_url", url)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrIntegratorNotFound
	}
	return nil
}

// SetWebhook sets where an integrator's events go and the sealed secret that
// signs them. Empty values clear both.
func SetWebhook(db *gorm.DB, integratorID, url, sealedSecret string) error {
	result := db.Model(&Integrator{}).Where("id = ?", integratorID).Updates(map[string]any{
		"webhook_url":              url,
		"webhook_secret_encrypted": sealedSecret,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrIntegratorNotFound
	}
	return nil
}

// LoadWebhook returns an integrator's webhook URL and sealed secret.
func LoadWebhook(db *gorm.DB, integratorID string) (url, sealedSecret string, err error) {
	var row Integrator
	err = db.Select("webhook_url", "webhook_secret_encrypted").
		First(&row, "id = ?", integratorID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", "", ErrIntegratorNotFound
	}
	return row.WebhookURL, row.WebhookSecretEncrypted, err
}

// OwedIntegrators lists the integrators with a report due now.
func OwedIntegrators(db *gorm.DB, now time.Time) ([]string, error) {
	// The order is looked up per owed row, not joined. Postgres keeps no
	// statistics for a partial index, so it overestimates how many rows are owed
	// and a join reads all of stellar_orders. This costs what is owed, however
	// long the history.
	var ids []string
	err := db.Raw(`SELECT DISTINCT integrator_id FROM (
		SELECT (SELECT o.integrator_id FROM stellar_orders o WHERE o.id = e.order_id) AS integrator_id
		  FROM stellar_order_status_events e WHERE `+owedEvents+` AND `+dueNow+`
		) owed WHERE integrator_id IS NOT NULL`, now).
		Scan(&ids).Error
	return ids, err
}

// OwedEventsFor returns an integrator's due reports, oldest first.
func OwedEventsFor(db *gorm.DB, integratorID string, now time.Time, limit int) ([]StatusEvent, error) {
	var events []StatusEvent
	err := db.Where(owedEvents).Where(dueNow, now).
		Where("order_id IN (SELECT id FROM stellar_orders WHERE integrator_id = ?)", integratorID).
		Order("at ASC, id ASC").
		Limit(limit).
		Find(&events).Error
	return events, err
}

// SkipOwedEvents closes the reports of an integrator with no webhook. It polls
// instead, so nothing is owed, and leaving them open would have every pass read
// them again for as long as the integrator exists.
func SkipOwedEvents(db *gorm.DB, integratorID string, now time.Time) error {
	return db.Model(&StatusEvent{}).Where(owedEvents).
		Where("order_id IN (SELECT id FROM stellar_orders WHERE integrator_id = ?)", integratorID).
		Update("notify_skipped_at", now).Error
}

// publicReasons is what an integrator is told when an order moved for a reason.
var publicReasons = map[string]string{
	StatePayoutQueued: "The payout attempt failed and will be retried.",
	StateRefundQueued: "The payout could not be completed, so the deposit is being refunded.",
}

// PublicReason is the explanation an integrator may see for a move. The stored
// reason is for Linq: it quotes payout providers and internal URLs. A state with
// no message here shows nothing, so a reason added later stays hidden until
// someone decides what to say.
func PublicReason(to, reason string) string {
	if reason == "" {
		return ""
	}
	return publicReasons[to]
}
