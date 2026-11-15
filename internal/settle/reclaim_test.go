package settle

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

// A reclaim moves no USDC, so its hash must never land in sweep_tx_hash:
// reconciliation would check it as a sweep and report it, every cycle.
func TestAReclaimIsRecordedAsAReclaimNotASweep(t *testing.T) {
	db := testDB(t)
	w := &ChainWorker{DB: db, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	for name, hash := range map[string]string{"closed": "merge_tx", "nothing to close": ""} {
		t.Run(name, func(t *testing.T) {
			order := store.Order{ID: "ord_" + name, IdempotencyKey: "idem_" + name, Status: store.StateExpired}
			if err := db.Create(&order).Error; err != nil {
				t.Fatalf("seed: %v", err)
			}
			w.recordReclaim(&order, hash)

			var got store.Order
			if err := db.First(&got, "id = ?", order.ID).Error; err != nil {
				t.Fatalf("read: %v", err)
			}
			if !got.ReservesReclaimed {
				t.Error("not marked reclaimed")
			}
			if got.ReclaimTxHash != hash {
				t.Errorf("reclaim_tx_hash = %q, want %q", got.ReclaimTxHash, hash)
			}
			if got.SweepTxHash != "" {
				t.Errorf("sweep_tx_hash = %q, want empty", got.SweepTxHash)
			}
		})
	}
}
