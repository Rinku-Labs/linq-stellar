package payout

import (
	"errors"
	"testing"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

// The fake pays whatever the worker sends, and answers for it afterwards,
// including after a restart.
func TestTheFakePaysWhatItIsSent(t *testing.T) {
	db := newTestDB(t)
	order := queuedOrder(t, db, false)
	pass(t, newWorker(db, Fake{DB: db}, true))

	got := reload(t, db, order.ID)
	if got.Status != store.StateDisbursed && got.Status != store.StateSweepQueued {
		t.Fatalf("status %s, want disbursed", got.Status)
	}
	if got.PayoutReference != "fake_ord_1" || got.PayoutProvider != FakeProvider {
		t.Errorf("payout recorded as %q via %q", got.PayoutReference, got.PayoutProvider)
	}
	if list := attempts(t, db, order.ID); len(list) != 1 || list[0].Outcome != store.PayoutPaid || list[0].Reference != "ord_1" {
		t.Errorf("attempts: %+v", list)
	}

	// A new Fake is a restarted worker: it still knows the payout.
	res, err := Fake{DB: db}.Status("ord_1")
	if err != nil || res.Status != StatusPaid || res.Reference != "fake_ord_1" || res.Provider != FakeProvider ||
		res.AmountNGN == nil || *res.AmountNGN != 1500 {
		t.Errorf("status of a paid reference: %+v %v", res, err)
	}
	if _, err := (Fake{DB: db}).Status("ord_1-p2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a reference never sent: %v, want ErrNotFound", err)
	}
}

// An induced failure never reaches a provider, so it fails with the fake too,
// and the fake never claims to have paid it.
func TestInducedFailuresStillFailWithTheFake(t *testing.T) {
	t.Run("transient", func(t *testing.T) {
		db := newTestDB(t)
		order := queuedOrder(t, db, true)
		if err := store.InducePayoutFailure(db, order.ID, store.InducedTransient); err != nil {
			t.Fatal(err)
		}
		w := newWorker(db, Fake{DB: db}, true)
		pass(t, w)
		if got := reload(t, db, order.ID); got.Status != store.StatePayoutQueued {
			t.Fatalf("after the induced failure: %s, want payout_queued for a retry", got.Status)
		}
		pass(t, w)
		if got := reload(t, db, order.ID); got.Status != store.StateDisbursed && got.Status != store.StateSweepQueued {
			t.Fatalf("after the retry: %s, want disbursed", got.Status)
		}
		list := attempts(t, db, order.ID)
		if len(list) != 2 || !list[0].Induced || list[1].Reference != "ord_1-p2" || list[1].Outcome != store.PayoutPaid {
			t.Errorf("attempts: %+v", list)
		}
		if _, err := (Fake{DB: db}).Status("ord_1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("the induced attempt was never sent: %v", err)
		}
		if res, err := (Fake{DB: db}).Status("ord_1-p2"); err != nil || res.Status != StatusPaid {
			t.Errorf("the retry: %+v %v", res, err)
		}
	})

	t.Run("terminal", func(t *testing.T) {
		db := newTestDB(t)
		order := queuedOrder(t, db, true)
		if err := store.InducePayoutFailure(db, order.ID, store.InducedTerminal); err != nil {
			t.Fatal(err)
		}
		w := newWorker(db, Fake{DB: db}, true)
		for i := 0; i < 3; i++ {
			pass(t, w)
		}
		if got := reload(t, db, order.ID); got.Status != store.StateRefundQueued {
			t.Fatalf("after three induced failures: %s, want refund_queued", got.Status)
		}
		if _, err := (Fake{DB: db}).Status(order.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("nothing was ever sent: %v", err)
		}
	})
}

func TestAFixedRateIsUsedAsIs(t *testing.T) {
	if rate, err := FixedRate(1500).Rate(); err != nil || rate != 1500 {
		t.Errorf("rate %v %v", rate, err)
	}
	if _, err := FixedRate(0).Rate(); err == nil {
		t.Error("no rate must mean no order")
	}
}
