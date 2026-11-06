package store

import (
	"time"

	"gorm.io/gorm"
)

// DefaultRateLimitPerMinute applies to a key unless its integrator has an
// override.
const DefaultRateLimitPerMinute = 120

// rateCounterRetention is how long a minute's count is kept. A window is only
// read while it is current, so this is tidiness rather than correctness.
const rateCounterRetention = time.Hour

// RateCounter is how many requests one key made in one clock minute. It lives
// in Postgres because this service has no Redis and runs on more than one
// instance: a counter in memory would let each instance allow the full limit.
type RateCounter struct {
	KeyID       string    `gorm:"primaryKey"`
	WindowStart time.Time `gorm:"primaryKey"`
	Count       int       `gorm:"not null"`
}

func (RateCounter) TableName() string { return "api_rate_counters" }

// HitRateCounter counts one request against a key's window and returns the
// total so far. The upsert is one atomic statement, so concurrent requests on
// any instance each see their own total and none is lost. Refused requests
// count too; the window resets either way.
func HitRateCounter(db *gorm.DB, keyID string, windowStart time.Time) (int, error) {
	var count int
	err := db.Raw(`
		INSERT INTO api_rate_counters (key_id, window_start, count)
		VALUES (?, ?, 1)
		ON CONFLICT (key_id, window_start)
		DO UPDATE SET count = api_rate_counters.count + 1
		RETURNING count`, keyID, windowStart).Scan(&count).Error
	return count, err
}

// PruneRateCounters deletes counters older than an hour.
func PruneRateCounters(db *gorm.DB, now time.Time) (int64, error) {
	result := db.Where("window_start < ?", now.Add(-rateCounterRetention)).Delete(&RateCounter{})
	return result.RowsAffected, result.Error
}
