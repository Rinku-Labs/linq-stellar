package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Rinku-Labs/linq-stellar/docs"
	"github.com/Rinku-Labs/linq-stellar/internal/apikey"
	"github.com/Rinku-Labs/linq-stellar/internal/notify"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"gorm.io/gorm"
)

// The public settlement API: /v1 for integrators, each with their own keys, and
// /admin for issuing and revoking those keys.
//
// /v1/orders runs the same handlers as the legacy /orders. The only difference
// is who the caller is, which comes from the request context, so the two cannot
// drift apart.

const (
	defaultEventLimit = 100
	maxEventLimit     = 1000

	// maxBodyBytes is far above any real order and far below anything worth
	// holding in memory for a stranger.
	maxBodyBytes  = 64 << 10
	maxNameLength = 200
)

// caller is who a request was authenticated as. keyID is empty on the legacy
// routes, where the shared key belongs to no row in api_keys.
type caller struct {
	integratorID string
	keyID        string
}

// legacy is true only for the shared key. Linq-B2B sends that with the rate it
// fetched from the same source, which no issued key ever may, and it is the only
// caller shown the raw reasons behind a failed payout.
func (c caller) legacy() bool { return c.keyID == "" }

type callerKey struct{}

func withCaller(r *http.Request, c caller) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), callerKey{}, c))
}

func callerOf(r *http.Request) caller {
	c, _ := r.Context().Value(callerKey{}).(caller)
	return c
}

func integratorID(r *http.Request) string { return callerOf(r).integratorID }

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// keyMode is the flavour of key this deployment issues.
func (s *Server) keyMode() string { return apikey.Mode(s.Chain.NetworkPassphrase()) }

func (s *Server) publicRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/orders", s.requireIntegratorKey(s.handleCreateOrder))
	mux.HandleFunc("GET /v1/orders/{id}", s.requireIntegratorKey(s.handleOrderStatus))
	mux.HandleFunc("POST /v1/keys/rotate", s.requireIntegratorKey(s.handleRotateOwnKey))

	mux.HandleFunc("GET /docs", s.handleDocs)
	mux.HandleFunc("GET /docs/{$}", s.handleDocs)
	mux.HandleFunc("GET /docs/README.md", serveDocFile("text/markdown; charset=utf-8", docs.Guide))
	mux.HandleFunc("GET /docs/openapi.yaml", serveDocFile("application/yaml; charset=utf-8", docs.OpenAPI))

	mux.HandleFunc("POST /admin/integrators", s.requireAdminKey(s.handleCreateIntegrator))
	mux.HandleFunc("PUT /admin/integrators/{id}/rate-limit", s.requireAdminKey(s.handleSetRateLimit))
	mux.HandleFunc("PUT /admin/integrators/{id}/webhook", s.requireAdminKey(s.handleSetWebhook))
	mux.HandleFunc("POST /admin/integrators/{id}/keys", s.requireAdminKey(s.handleIssueKey))
	mux.HandleFunc("POST /admin/keys/{id}/rotate", s.requireAdminKey(s.handleAdminRotateKey))
	mux.HandleFunc("POST /admin/keys/{id}/revoke", s.requireAdminKey(s.handleRevokeKey))
	mux.HandleFunc("GET /admin/credential-events", s.requireAdminKey(s.handleCredentialEvents))
	mux.HandleFunc("POST /admin/orders/{id}/induce-payout-failure", s.requireAdminKey(s.handleInducePayoutFailure))
}

// handleInducePayoutFailure makes a Linq test order's payout fail on purpose,
// to demonstrate the retry and refund paths. See store.InducePayoutFailure.
func (s *Server) handleInducePayoutFailure(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	id := r.PathValue("id")
	err := store.InducePayoutFailure(s.DB, id, body.Kind)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnknownInducedKind):
		writeError(w, http.StatusBadRequest, "kind must be transient or terminal")
		return
	case errors.Is(err, store.ErrOrderNotFound):
		writeError(w, http.StatusNotFound, "order not found")
		return
	case errors.Is(err, store.ErrFailureInjectionOff):
		writeError(w, http.StatusForbidden, "failure injection is not allowed for this order's integrator")
		return
	case errors.Is(err, store.ErrFailureInjectionTooLate):
		writeError(w, http.StatusConflict, "the order's payout has already started, or a failure is already induced")
		return
	default:
		s.Log.Error("could not induce a payout failure", "order", id, "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not induce the failure right now; try again shortly")
		return
	}
	s.Log.Warn("payout failure induced", "order", id, "kind", body.Kind)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "inducedFailure": body.Kind})
}

// requireIntegratorKey authenticates an issued key, then counts the request
// against it. In that order on purpose: only someone holding the key can spend
// its allowance, so a stranger who knows a prefix cannot lock its owner out.
func (s *Server) requireIntegratorKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		key, ok := s.authenticate(w, r)
		if !ok || !s.allowRequest(w, r, key) {
			return
		}
		next(w, withCaller(r, caller{integratorID: key.IntegratorID, keyID: key.ID}))
	}
}

// authenticate resolves X-API-Key to a usable key, or writes the refusal.
//
// A key that fails the hash check records nothing: it proves nothing about the
// key it named. A revoked or expired key does, because whoever sent it held the
// real secret, and that is the use credential_events exists to show.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (store.APIKey, bool) {
	const invalid = "invalid or missing X-API-Key"

	presented := r.Header.Get("X-API-Key")
	prefix, err := apikey.Prefix(presented)
	if err != nil {
		writeError(w, http.StatusUnauthorized, invalid)
		return store.APIKey{}, false
	}
	key, err := store.FindKey(s.DB, prefix)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeError(w, http.StatusUnauthorized, invalid)
		return store.APIKey{}, false
	}
	if err != nil {
		s.Log.Error("could not look up an api key", "prefix", prefix, "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not check the key right now; try again shortly")
		return store.APIKey{}, false
	}
	if !apikey.Matches(presented, key.SecretHash) {
		writeError(w, http.StatusUnauthorized, invalid)
		return store.APIKey{}, false
	}

	now := s.now()
	switch {
	case key.RevokedAt != nil:
		s.recordRejection(key, store.CredentialRejectedRevoked, now)
		writeError(w, http.StatusUnauthorized, "this key has been revoked")
		return store.APIKey{}, false
	case key.Expired(now):
		s.recordRejection(key, store.CredentialRejectedExpired, now)
		writeError(w, http.StatusUnauthorized, "this key has expired")
		return store.APIKey{}, false
	case key.Integrator.Status != store.IntegratorActive:
		writeError(w, http.StatusForbidden, "this integrator is suspended")
		return store.APIKey{}, false
	}

	if err := store.TouchKey(s.DB, key, now); err != nil {
		s.Log.Warn("could not record key usage", "key", key.ID, "error", err)
	}
	return key, true
}

func (s *Server) recordRejection(key store.APIKey, action string, now time.Time) {
	// Every use is logged. The table keeps one row a minute; see RecordRejection.
	s.Log.Info("rejected key used", "integrator", key.IntegratorID, "key", key.ID, "action", action)
	if err := store.RecordRejection(s.DB, key, action, now); err != nil {
		s.Log.Error("could not record a rejected key", "key", key.ID, "action", action, "error", err)
	}
}

// allowRequest counts a request against its key's current minute and writes the
// rate limit headers. It reports whether the request may go on.
//
// A counter that cannot be written refuses the request. The counter is in the
// same database as the order the request wants to create, so letting it through
// would buy nothing and would leave a gap in a limit we claim always holds.
func (s *Server) allowRequest(w http.ResponseWriter, r *http.Request, key store.APIKey) bool {
	limit := store.DefaultRateLimitPerMinute
	if override := key.Integrator.RateLimitPerMinute; override != nil {
		limit = *override
	}

	now := s.now()
	window := now.Truncate(time.Minute)
	count, err := store.HitRateCounter(s.DB, key.ID, window)
	if err != nil {
		s.Log.Error("could not count a request", "key", key.ID, "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not process the request right now; try again shortly")
		return false
	}

	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(max(limit-count, 0)))
	if count <= limit {
		return true
	}

	// Whole seconds, rounded up, so a caller who waits exactly this long lands
	// in the next window.
	retryAfter := int((window.Add(time.Minute).Sub(now) + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))

	// This line is the proof of enforcement; the counter row is deleted within
	// the hour. The key ID identifies the key and contains none of its secret.
	s.Log.Warn("rate limit refused",
		"key", key.ID,
		"integrator", key.IntegratorID,
		"at", now.Format(time.RFC3339Nano),
		"limit", limit,
		"count", count,
		"window", window.Format(time.RFC3339),
		"method", r.Method,
		"path", r.URL.Path)
	writeError(w, http.StatusTooManyRequests, "rate limit exceeded; see Retry-After")
	return false
}

// requireAdminKey guards /admin. An unset ADMIN_API_KEY matches nothing rather
// than matching an empty header.
func (s *Server) requireAdminKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		provided := r.Header.Get("X-Admin-Key")
		if s.AdminAPIKey == "" || provided == "" ||
			!apikey.Matches(provided, apikey.Hash(s.AdminAPIKey)) {
			writeError(w, http.StatusUnauthorized, "invalid or missing X-Admin-Key")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleRotateOwnKey(w http.ResponseWriter, r *http.Request) {
	keyID := callerOf(r).keyID
	s.rotate(w, keyID, store.ActorKey(keyID))
}

func (s *Server) handleAdminRotateKey(w http.ResponseWriter, r *http.Request) {
	s.rotate(w, r.PathValue("id"), store.ActorAdmin)
}

func (s *Server) rotate(w http.ResponseWriter, keyID, actor string) {
	now := s.now()
	issued, old, err := store.RotateKey(s.DB, keyID, s.keyMode(), actor, now)
	if err != nil {
		s.writeCredentialError(w, "rotate the key", err)
		return
	}
	s.Log.Info("api key rotated", "integrator", old.IntegratorID, "key", old.ID,
		"replacedBy", issued.ID, "actor", actor, "oldKeyExpiresAt", old.ExpiresAt)
	writeIssuedKey(w, issued, map[string]any{
		"previousKey": map[string]any{"id": old.ID, "expiresAt": old.ExpiresAt},
	})
}

func (s *Server) handleCreateIntegrator(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name               string `json:"name"`
		RateLimitPerMinute *int   `json:"rateLimitPerMinute"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Name == "" || len(body.Name) > maxNameLength {
		writeError(w, http.StatusBadRequest, "name is required, up to 200 characters")
		return
	}
	if !validLimit(body.RateLimitPerMinute) {
		writeError(w, http.StatusBadRequest, "rateLimitPerMinute must be above zero")
		return
	}
	integrator, err := store.CreateIntegrator(s.DB, body.Name, body.RateLimitPerMinute)
	if err != nil {
		s.writeCredentialError(w, "create the integrator", err)
		return
	}
	s.Log.Info("integrator created", "integrator", integrator.ID, "name", integrator.Name)
	writeJSON(w, http.StatusCreated, integrator)
}

// handleSetRateLimit sets an integrator's override. A null clears it.
func (s *Server) handleSetRateLimit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RateLimitPerMinute *int `json:"rateLimitPerMinute"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !validLimit(body.RateLimitPerMinute) {
		writeError(w, http.StatusBadRequest, "rateLimitPerMinute must be above zero")
		return
	}
	id := r.PathValue("id")
	if err := store.SetRateLimit(s.DB, id, body.RateLimitPerMinute); err != nil {
		s.writeCredentialError(w, "set the rate limit", err)
		return
	}
	s.Log.Info("integrator rate limit set", "integrator", id, "perMinute", body.RateLimitPerMinute)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                 id,
		"rateLimitPerMinute": body.RateLimitPerMinute,
	})
}

func validLimit(limit *int) bool { return limit == nil || *limit > 0 }

// handleSetWebhook points an integrator's events at a URL. The first URL mints the
// secret that signs them, shown only in that reply; a later one keeps it, so a
// move of endpoint does not break the receiver's signature check. rotateSecret
// replaces the secret with no gap in the webhook. An empty url clears both.
// Linq-B2B is refused: its webhook comes from MERCHANT_WEBHOOK_URL.
func (s *Server) handleSetWebhook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL          string `json:"url"`
		RotateSecret bool   `json:"rotateSecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	id := r.PathValue("id")
	if id == store.LegacyIntegratorID {
		writeError(w, http.StatusConflict, "this integrator's webhook is set by MERCHANT_WEBHOOK_URL")
		return
	}

	if body.URL == "" {
		if body.RotateSecret {
			writeError(w, http.StatusBadRequest, "rotateSecret needs a url")
			return
		}
		if err := store.ClearWebhook(s.DB, id, s.now()); err != nil {
			s.writeCredentialError(w, "clear the webhook", err)
			return
		}
		s.Log.Info("integrator webhook cleared", "integrator", id)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "webhookUrl": nil})
		return
	}

	if err := notify.ValidateWebhookURL(body.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, existing, err := store.LoadWebhook(s.DB, id)
	if err != nil {
		s.writeCredentialError(w, "load the webhook", err)
		return
	}
	if existing != "" && !body.RotateSecret {
		if err := store.SetWebhookURL(s.DB, id, body.URL); err != nil {
			s.writeCredentialError(w, "set the webhook", err)
			return
		}
		s.Log.Info("integrator webhook moved", "integrator", id)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "webhookUrl": body.URL})
		return
	}

	secret, err := apikey.NewWebhookSecret()
	if err != nil {
		s.writeCredentialError(w, "create the webhook secret", err)
		return
	}
	// EncryptSeed seals any secret; the name is for its first use.
	sealed, err := s.Chain.EncryptSeed(secret)
	if err != nil {
		s.writeCredentialError(w, "seal the webhook secret", err)
		return
	}
	if err := store.SetWebhook(s.DB, id, body.URL, sealed); err != nil {
		s.writeCredentialError(w, "set the webhook", err)
		return
	}

	s.Log.Info("integrator webhook set", "integrator", id)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"webhookUrl":    body.URL,
		"webhookSecret": secret,
	})
}

func (s *Server) handleIssueKey(w http.ResponseWriter, r *http.Request) {
	issued, err := store.IssueKey(s.DB, r.PathValue("id"), s.keyMode(), store.ActorAdmin, s.now())
	if err != nil {
		s.writeCredentialError(w, "issue the key", err)
		return
	}
	s.Log.Info("api key issued", "integrator", issued.IntegratorID, "key", issued.ID)
	writeIssuedKey(w, issued, nil)
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	key, also, err := store.RevokeKey(s.DB, r.PathValue("id"), store.ActorAdmin, s.now())
	if err != nil {
		s.writeCredentialError(w, "revoke the key", err)
		return
	}
	s.Log.Info("api key revoked", "integrator", key.IntegratorID, "key", key.ID, "alsoRevoked", also)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           key.ID,
		"integratorId": key.IntegratorID,
		"revokedAt":    key.RevokedAt,
		"alsoRevoked":  also,
	})
}

func (s *Server) handleCredentialEvents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := defaultEventLimit
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > maxEventLimit {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 1000")
			return
		}
		limit = parsed
	}
	events, err := store.ListCredentialEvents(s.DB, store.CredentialEventFilter{
		IntegratorID: query.Get("integratorId"),
		KeyID:        query.Get("keyId"),
		Limit:        limit,
	})
	if err != nil {
		s.writeCredentialError(w, "list credential events", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// writeIssuedKey is the one place a full key leaves this service. Nothing can
// show it again, so it must not be kept by anything in between either.
func writeIssuedKey(w http.ResponseWriter, issued store.IssuedKey, extra map[string]any) {
	body := map[string]any{
		"id":           issued.ID,
		"integratorId": issued.IntegratorID,
		"keyPrefix":    issued.KeyPrefix,
		"key":          issued.Full,
		"createdAt":    issued.CreatedAt,
	}
	for name, value := range extra {
		body[name] = value
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, body)
}

func (s *Server) writeCredentialError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, store.ErrIntegratorNotFound):
		writeError(w, http.StatusNotFound, "integrator not found")
	case errors.Is(err, store.ErrKeyNotFound):
		writeError(w, http.StatusNotFound, "key not found")
	case errors.Is(err, store.ErrLegacyIntegrator):
		writeError(w, http.StatusConflict, "this integrator uses the shared key and cannot hold issued keys")
	case errors.Is(err, store.ErrKeyNotRotatable):
		writeError(w, http.StatusConflict, "this key is revoked, expired or already rotated")
	default:
		s.Log.Error("credential operation failed", "action", action, "error", err)
		writeError(w, http.StatusInternalServerError, "could not "+action)
	}
}
