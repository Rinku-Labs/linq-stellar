package store

import (
	"errors"
	"testing"
	"time"
)

func TestAResolutionIsWrittenOnceAndOnlyForAProblem(t *testing.T) {
	db := poolDB(t)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	cycle, err := StartCycle(db, now)
	if err != nil {
		t.Fatal(err)
	}
	passed := ReconciliationItem{CycleID: cycle.ID, OrderID: "o1", CheckName: "deposit_tx", Result: ReconcileOK}
	problem := ReconciliationItem{CycleID: cycle.ID, OrderID: "o1", CheckName: "sweep_tx", Result: ReconcileMismatch}
	for _, item := range []*ReconciliationItem{&passed, &problem} {
		if err := AddReconciliationItem(db, item); err != nil {
			t.Fatal(err)
		}
	}

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

	settled, err := SettledChecks(db, []string{"o1", "o2"})
	if err != nil {
		t.Fatal(err)
	}
	if !settled["o1"]["deposit_tx"] || !settled["o1"]["sweep_tx"] || len(settled["o2"]) != 0 {
		t.Errorf("settled checks: %v", settled)
	}
}

func TestOnlyCyclesThatRanToTheEndCount(t *testing.T) {
	db := poolDB(t)
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
}
