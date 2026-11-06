package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

// Bounds on a resolution note, which a reviewer reads on the reconciliation
// page.
const (
	maxResolutionLen = 2000
	maxResolvedByLen = 100
)

// handleResolveReconciliationItem records a person's explanation of a
// reconciliation mismatch: why it is not a problem, or what was done about it.
// It changes nothing else. The engine stops flagging an order's check once it
// has a resolution, so the note is what turns a known mismatch from noise into
// a record.
func (s *Server) handleResolveReconciliationItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "reconciliation item not found")
		return
	}
	var body struct {
		Resolution string `json:"resolution"`
		ResolvedBy string `json:"resolvedBy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resolution := strings.TrimSpace(body.Resolution)
	by := strings.TrimSpace(body.ResolvedBy)
	switch {
	case resolution == "" || utf8.RuneCountInString(resolution) > maxResolutionLen:
		writeError(w, http.StatusBadRequest, "resolution is required, up to 2000 characters")
		return
	case by == "" || utf8.RuneCountInString(by) > maxResolvedByLen:
		writeError(w, http.StatusBadRequest, "resolvedBy is required, up to 100 characters")
		return
	}

	item, err := store.ResolveReconciliationItem(s.DB, id, resolution, by, s.now())
	switch {
	case errors.Is(err, store.ErrReconcileItemNotFound):
		writeError(w, http.StatusNotFound, "reconciliation item not found")
		return
	case errors.Is(err, store.ErrNothingToResolve):
		writeError(w, http.StatusConflict, "this check passed; there is nothing to resolve")
		return
	case errors.Is(err, store.ErrAlreadyResolved):
		writeError(w, http.StatusConflict, "this item is already resolved")
		return
	case err != nil:
		s.Log.Error("could not resolve a reconciliation item", "item", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not resolve the item")
		return
	}
	s.Log.Info("reconciliation item resolved", "item", id, "order", item.OrderID,
		"check", item.CheckName, "by", by)
	writeJSON(w, http.StatusOK, map[string]any{"item": item})
}
