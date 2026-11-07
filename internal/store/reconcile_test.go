package store

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

func addItems(t *testing.T, db *gorm.DB, items ...*ReconciliationItem) {
	t.Helper()
	for _, item := range items {
		if err := AddReconciliationItem(db, item); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAResolutionIsWrittenOnceAndOnlyForAProblem(t *testing.T) {
	engines(t, func(t *testing.T, open func() *gorm.DB) {
		db := open()
		now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
		cycle, err := StartCycle(db, now)
		if err != nil {
			t.Fatal(err)
		}
		passed := ReconciliationItem{CycleID: cycle.ID, OrderID: "o1", CheckName: "deposit_tx", Result: ReconcileOK}
		problem := ReconciliationItem{CycleID: cycle.ID, OrderID: "o1", CheckName: "sweep_tx", Result: ReconcileMismatch}
		addItems(t, db, &passed, &problem)

		if _, err := ResolveReconciliationItem(db, passed.ID, "fine", "ify", now); !errors.Is(err, ErrNothingToResolve) {
			t.Errorf("resolving a pass: %v", err)
		}
		item, err := ResolveReconciliationItem(db, problem.ID, "Induced for the demo", "ify", now)
		if err != nil || item.Resolution != "Induced for the demo" || item.ResolvedBy != "ify" || item.ResolvedAt == nil {
			t.Fatalf("resolve: %+v %v", item, err)
		}
		if _, err := ResolveReconciliationItem(db, problem.ID, "Something else", "someone", now); !errors.Is(err, ErrAlreadyResolved) {
			t.Errorf("second resolution: %v", err)
		}
		var kept ReconciliationItem
		db.First(&kept, "id = ?", problem.ID)
		if kept.Resolution != "Induced for the demo" {
			t.Errorf("the first note was overwritten: %q", kept.Resolution)
		}
		if _, err := ResolveReconciliationItem(db, 9999, "x", "y", now); !errors.Is(err, ErrReconcileItemNotFound) {
			t.Errorf("unknown item: %v", err)
		}
	})
}

// An unexplained mismatch is recorded again every cycle. Its note covers every
// copy of that same problem, and nothing else.
func TestAResolutionCoversEveryCopyOfTheSameProblem(t *testing.T) {
	engines(t, func(t *testing.T, open func() *gorm.DB) {
		db := open()
		now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
		cycle, _ := StartCycle(db, now)
		copyOf := func(actual string) *ReconciliationItem {
			return &ReconciliationItem{CycleID: cycle.ID, OrderID: "o1", CheckName: "payout", Result: ReconcileMismatch,
				Expected: "no NGN paid", Actual: actual}
		}
		// A copy someone resolved before keeps its own note.
		noted := copyOf("queued")
		addItems(t, db, noted)
		if _, err := ResolveReconciliationItem(db, noted.ID, "Already noted", "tanta", now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		first, second := copyOf("queued"), copyOf("queued")
		otherAnswer := copyOf("paid")
		otherOrder := &ReconciliationItem{CycleID: cycle.ID, OrderID: "o2", CheckName: "payout", Result: ReconcileMismatch, Actual: "queued"}
		otherCheck := &ReconciliationItem{CycleID: cycle.ID, OrderID: "o1", CheckName: "account_closed", Result: ReconcileMismatch, Actual: "queued"}
		anError := &ReconciliationItem{CycleID: cycle.ID, OrderID: "o1", CheckName: "payout", Result: ReconcileError, Actual: "queued"}
		addItems(t, db, first, second, otherAnswer, otherOrder, otherCheck, anError)

		if _, err := ResolveReconciliationItem(db, second.ID, "Nomba pending, watching", "ify", now); err != nil {
			t.Fatal(err)
		}
		note := func(id int64) string {
			var item ReconciliationItem
			db.First(&item, "id = ?", id)
			return item.Resolution
		}
		if note(first.ID) != "Nomba pending, watching" || note(second.ID) != "Nomba pending, watching" {
			t.Errorf("every copy of the problem takes the note: %q, %q", note(first.ID), note(second.ID))
		}
		if note(noted.ID) != "Already noted" {
			t.Errorf("a copy with its own note keeps it: %q", note(noted.ID))
		}
		for name, id := range map[string]int64{"another answer": otherAnswer.ID, "another order": otherOrder.ID,
			"another check": otherCheck.ID, "an error": anError.ID} {
			if note(id) != "" {
				t.Errorf("%s was resolved too", name)
			}
		}
	})
}

// A pass settles any check. A resolved mismatch settles only a check whose
// answer cannot change. A resolved error settles nothing.
func TestOnlyAPassOrAResolvedFinalCheckIsSettled(t *testing.T) {
	engines(t, func(t *testing.T, open func() *gorm.DB) {
		db := open()
		now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
		cycle, _ := StartCycle(db, now)
		item := func(check, result string) *ReconciliationItem {
			return &ReconciliationItem{CycleID: cycle.ID, OrderID: "o1", CheckName: check, Result: result, Actual: check}
		}
		passedPayout := item("payout", ReconcileOK)
		resolvedSweep := item("sweep_tx", ReconcileMismatch)
		resolvedClosed := item("account_closed", ReconcileMismatch)
		resolvedError := item("deposit_tx", ReconcileError)
		openSweep := item("refund_tx", ReconcileMismatch)
		addItems(t, db, passedPayout, resolvedSweep, resolvedClosed, resolvedError, openSweep)
		for _, it := range []*ReconciliationItem{resolvedSweep, resolvedClosed, resolvedError} {
			if _, err := ResolveReconciliationItem(db, it.ID, "explained", "ify", now); err != nil {
				t.Fatal(err)
			}
		}

		settled, err := SettledChecks(db, []string{"o1", "o2"}, []string{"deposit_tx", "sweep_tx", "refund_tx"})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{"payout": true, "sweep_tx": true}
		if len(settled["o1"]) != len(want) || !settled["o1"]["payout"] || !settled["o1"]["sweep_tx"] || len(settled["o2"]) != 0 {
			t.Errorf("settled %v, want %v", settled, want)
		}

		explained, err := ExplainedAlready(db, ReconciliationItem{OrderID: "o1", CheckName: "account_closed", Actual: "account_closed"})
		if err != nil || !explained {
			t.Errorf("the resolved answer is explained: %v %v", explained, err)
		}
		for name, it := range map[string]ReconciliationItem{
			"another answer":   {OrderID: "o1", CheckName: "account_closed", Actual: "the account still exists"},
			"another order":    {OrderID: "o2", CheckName: "account_closed", Actual: "account_closed"},
			"an open one":      {OrderID: "o1", CheckName: "refund_tx", Actual: "refund_tx"},
			"a resolved error": {OrderID: "o1", CheckName: "deposit_tx", Actual: "deposit_tx"},
		} {
			if explained, _ := ExplainedAlready(db, it); explained {
				t.Errorf("%s counts as explained", name)
			}
		}
	})
}

func TestOnlyCyclesThatRanToTheEndCount(t *testing.T) {
	engines(t, func(t *testing.T, open func() *gorm.DB) {
		db := open()
		now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
		done, _ := StartCycle(db, now)
		if err := FinishCycle(db, done, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		stopped, _ := StartCycle(db, now)
		stopped.Error = "stopped early: the worker is shutting down"
		if err := FinishCycle(db, stopped, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := StartCycle(db, now); err != nil { // still running
			t.Fatal(err)
		}
		if n, err := CompletedCycles(db); err != nil || n != 1 {
			t.Errorf("completed cycles %d, %v", n, err)
		}
		latest, err := LatestCycles(db, 2)
		if err != nil || len(latest) != 2 || latest[0].ID <= latest[1].ID {
			t.Errorf("latest cycles: %+v %v", latest, err)
		}
	})
}
