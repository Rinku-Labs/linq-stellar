package notify

import (
	"fmt"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

// newTestDB gives each test its own in-memory database.
//
// In-memory and named per test on purpose. These tests seed orders keyed by
// status, so a database shared across the package would have one test's
// fixtures show up in another's sweep and the failures would depend on test
// order. Nothing here can reach a real database.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:notify_%s?mode=memory&cache=shared", sanitize(t.Name()))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	t.Cleanup(func() {
		sql, err := db.DB()
		if err == nil {
			_ = sql.Close()
		}
	})
	return db
}

func sanitize(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
