package store

import "testing"

// A reclaim used to be recorded in sweep_tx_hash. Migrate moves it to
// reclaim_tx_hash, on expired, reclaimed orders only, and only once.
func TestMigrateMovesReclaimHashesOutOfTheSweepColumn(t *testing.T) {
	db := historyDB(t)
	rows := []Order{
		{ID: "reclaimed", Status: StateExpired, ReservesReclaimed: true, SweepTxHash: "merge"},
		// Marked reclaimed but settled: its hash is a real sweep and stays.
		{ID: "swept", Status: StateSettledInTreasury, ReservesReclaimed: true, SweepTxHash: "sweep"},
		{ID: "not_reclaimed", Status: StateExpired, SweepTxHash: "odd"},
		{ID: "already_moved", Status: StateExpired, ReservesReclaimed: true, ReclaimTxHash: "merge2"},
		{ID: "nothing_to_close", Status: StateExpired, ReservesReclaimed: true},
	}
	for i := range rows {
		rows[i].IdempotencyKey = rows[i].ID
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed %s: %v", rows[i].ID, err)
		}
	}

	for run := 1; run <= 2; run++ {
		if err := Migrate(db); err != nil {
			t.Fatalf("migrate, run %d: %v", run, err)
		}
		want := map[string][2]string{ // id: sweep, reclaim
			"reclaimed":        {"", "merge"},
			"swept":            {"sweep", ""},
			"not_reclaimed":    {"odd", ""},
			"already_moved":    {"", "merge2"},
			"nothing_to_close": {"", ""},
		}
		for id, w := range want {
			var o Order
			if err := db.First(&o, "id = ?", id).Error; err != nil {
				t.Fatalf("read %s: %v", id, err)
			}
			if o.SweepTxHash != w[0] || o.ReclaimTxHash != w[1] {
				t.Errorf("run %d, %s: sweep %q reclaim %q, want sweep %q reclaim %q",
					run, id, o.SweepTxHash, o.ReclaimTxHash, w[0], w[1])
			}
		}
	}
}
