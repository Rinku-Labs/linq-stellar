package store

import (
	"time"

	"gorm.io/gorm"
)

// StreamCursor remembers how far a Horizon stream has been consumed.
//
// This is the difference between a stream that is reliable and one that quietly
// loses money. Horizon closes long-lived connections routinely, and a stream
// reopened at "now" skips every payment that landed while it was down — with no
// error, no retry, and no way to tell it happened. Resuming from the last
// persisted cursor is what makes a reconnect safe.
type StreamCursor struct {
	// StreamID names the stream, e.g. "payments:GABC...".
	StreamID string `gorm:"primaryKey;size:255" json:"streamId"`
	// Cursor is Horizon's paging token for the last event handled.
	Cursor    string    `json:"cursor"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (StreamCursor) TableName() string { return "stellar_stream_cursors" }

// LoadCursor returns the last persisted cursor for a stream, or "" if the
// stream has never run. Callers starting from "" should begin at the order's
// creation point rather than at "now", so a deposit made before the stream
// first connected is not missed.
func LoadCursor(db *gorm.DB, streamID string) (string, error) {
	var c StreamCursor
	err := db.Where("stream_id = ?", streamID).First(&c).Error
	if err == gorm.ErrRecordNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return c.Cursor, nil
}

// SaveCursor records progress through a stream.
//
// Called after an event is handled, never before: a cursor saved ahead of the
// work it represents will skip that event on the next reconnect.
func SaveCursor(db *gorm.DB, streamID, cursor string) error {
	return db.Save(&StreamCursor{
		StreamID:  streamID,
		Cursor:    cursor,
		UpdatedAt: time.Now().UTC(),
	}).Error
}
