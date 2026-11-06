package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

func TestAnAdminResolvesAReconciliationMismatch(t *testing.T) {
	p := newPublicAPI(t)
	cycle, err := store.StartCycle(p.db, p.clock)
	if err != nil {
		t.Fatal(err)
	}
	problem := store.ReconciliationItem{CycleID: cycle.ID, OrderID: "o1", CheckName: "sweep_to_treasury", Result: store.ReconcileMismatch}
	passed := store.ReconciliationItem{CycleID: cycle.ID, OrderID: "o1", CheckName: "deposit_tx", Result: store.ReconcileOK}
	for _, item := range []*store.ReconciliationItem{&problem, &passed} {
		if err := store.AddReconciliationItem(p.db, item); err != nil {
			t.Fatal(err)
		}
	}
	path := func(id int64) string { return "/admin/reconciliation/items/" + itoa(id) + "/resolve" }
	note := `{"resolution":"Induced for the SD-11 demo","resolvedBy":"ify"}`

	if w := p.send(http.MethodPost, path(problem.ID), note, map[string]string{"X-Admin-Key": "wrong"}); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong admin key: %d", w.Code)
	}
	for body, want := range map[string]int{
		`{"resolution":"","resolvedBy":"ify"}`:    http.StatusBadRequest,
		`{"resolution":"fine","resolvedBy":"  "}`: http.StatusBadRequest,
		`not json`: http.StatusBadRequest,
	} {
		if w := p.asAdmin(http.MethodPost, path(problem.ID), body); w.Code != want {
			t.Errorf("%s: %d, want %d", body, w.Code, want)
		}
	}
	if w := p.asAdmin(http.MethodPost, path(passed.ID), note); w.Code != http.StatusConflict {
		t.Errorf("resolving a pass: %d", w.Code)
	}
	if w := p.asAdmin(http.MethodPost, path(424242), note); w.Code != http.StatusNotFound {
		t.Errorf("unknown item: %d", w.Code)
	}

	w := p.asAdmin(http.MethodPost, path(problem.ID), note)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", w.Code, w.Body)
	}
	var reply struct {
		Item store.ReconciliationItem `json:"item"`
	}
	if err := json.NewDecoder(w.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.Item.Resolution != "Induced for the SD-11 demo" || reply.Item.ResolvedBy != "ify" || reply.Item.ResolvedAt == nil {
		t.Errorf("reply: %+v", reply.Item)
	}
	if w := p.asAdmin(http.MethodPost, path(problem.ID), note); w.Code != http.StatusConflict {
		t.Errorf("second resolution: %d", w.Code)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
