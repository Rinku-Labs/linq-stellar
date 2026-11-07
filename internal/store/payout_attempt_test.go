package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// The induce guard is one conditional UPDATE, so it is checked on Postgres too.
func TestInducePayoutFailureGuard(t *testing.T) {
	engines(t, func(t *testing.T, open func() *gorm.DB) {
		db := open()
		db.Create(&Integrator{ID: "int_demo", Name: "Linq demo", AllowFailureInjection: true})
		db.Create(&Integrator{ID: "int_real", Name: "Customer"})
		db.Create(&Order{ID: "demo", IdempotencyKey: "a", IntegratorID: "int_demo", Status: StatePayoutQueued})
		db.Create(&Order{ID: "real", IdempotencyKey: "b", IntegratorID: "int_real", Status: StatePayoutQueued})
		db.Create(&Order{ID: "paying", IdempotencyKey: "c", IntegratorID: "int_demo", Status: StatePayoutProcessing})

		if err := InducePayoutFailure(db, "real", InducedTerminal); !errors.Is(err, ErrFailureInjectionOff) {
			t.Errorf("customer order: %v, want ErrFailureInjectionOff", err)
		}
		if err := InducePayoutFailure(db, "paying", InducedTerminal); !errors.Is(err, ErrFailureInjectionTooLate) {
			t.Errorf("payout under way: %v, want ErrFailureInjectionTooLate", err)
		}
		if err := InducePayoutFailure(db, "demo", InducedTransient); err != nil {
			t.Fatalf("demo order: %v", err)
		}
		if err := InducePayoutFailure(db, "demo", InducedTerminal); !errors.Is(err, ErrFailureInjectionTooLate) {
			t.Errorf("second time: %v, want ErrFailureInjectionTooLate", err)
		}
		if kind, err := InducedFailureOf(db, "demo"); err != nil || kind != InducedTransient {
			t.Errorf("demo kind = %q, %v; want transient", kind, err)
		}
		if kind, err := InducedFailureOf(db, "real"); err != nil || kind != "" {
			t.Errorf("customer kind = %q, %v; want none", kind, err)
		}

		if _, err := StartPayoutAttempt(db, "real", 1, "real"); err != nil {
			t.Fatal(err)
		}
		if _, err := StartPayoutAttempt(db, "real", 1, "real-again"); err == nil {
			t.Errorf("two attempts with the same number were both recorded")
		}
	})
}
