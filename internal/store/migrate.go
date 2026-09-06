package store

import "gorm.io/gorm"

// Migrate creates or updates the schema this service owns.
//
// Two tables: the settlement orders themselves, and the Horizon stream cursors
// that make reconnects safe. Nothing else — this service does not own merchant
// records or bank details beyond what it needs to settle one payment.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&Order{}, &StreamCursor{})
}
