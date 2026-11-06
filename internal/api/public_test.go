package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const testAdminKey = "test-admin-key"

// publicAPI is a server with /v1 switched on, its own database, a clock the test
// can move and a log it can read.
type publicAPI struct {
	*Server
	db    *gorm.DB
	clock time.Time
	logs  *bytes.Buffer
}

func newPublicAPI(t *testing.T) *publicAPI {
	t.Helper()
	s, _ := testServer(t)

	dsn := fmt.Sprintf("file:public_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if sql, err := db.DB(); err == nil {
			_ = sql.Close()
		}
	})

	p := &publicAPI{
		Server: s,
		db:     db,
		// Fifteen seconds into a minute, so a window boundary is in reach.
		clock: time.Date(2026, 11, 8, 12, 0, 15, 0, time.UTC),
		logs:  &bytes.Buffer{},
	}
	s.DB = db
	s.PublicAPIEnabled = true
	s.AdminAPIKey = testAdminKey
	s.Rates = &stubRate{rate: 1655}
	s.Now = func() time.Time { return p.clock }
	s.Log = slog.New(slog.NewTextHandler(p.logs, nil))
	return p
}

func (p *publicAPI) send(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	p.Routes().ServeHTTP(w, r)
	return w
}

func (p *publicAPI) asKey(key, method, path, body string) *httptest.ResponseRecorder {
	return p.send(method, path, body, map[string]string{"X-API-Key": key})
}

func (p *publicAPI) asAdmin(method, path, body string) *httptest.ResponseRecorder {
	return p.send(method, path, body, map[string]string{"X-Admin-Key": testAdminKey})
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return body
}

func (p *publicAPI) integrator(t *testing.T, name string) string {
	t.Helper()
	w := p.asAdmin(http.MethodPost, "/admin/integrators", fmt.Sprintf(`{"name":%q}`, name))
	if w.Code != http.StatusCreated {
		t.Fatalf("create integrator: %d %s", w.Code, w.Body.String())
	}
	return decode(t, w)["id"].(string)
}

// issue returns a new key's id and its full value.
func (p *publicAPI) issue(t *testing.T, integrator string) (id, key string) {
	t.Helper()
	w := p.asAdmin(http.MethodPost, "/admin/integrators/"+integrator+"/keys", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("issue key: %d %s", w.Code, w.Body.String())
	}
	body := decode(t, w)
	return body["id"].(string), body["key"].(string)
}

func (p *publicAPI) actions(t *testing.T, keyID string) []string {
	t.Helper()
	var events []store.CredentialEvent
	if err := p.db.Order("id").Find(&events, "key_id = ?", keyID).Error; err != nil {
		t.Fatalf("load events: %v", err)
	}
	actions := make([]string, 0, len(events))
	for _, event := range events {
		actions = append(actions, event.Action)
	}
	return actions
}

func wantActions(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("credential events = %v, want %v", got, want)
	}
}

func (p *publicAPI) seedPool(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		address, seed, err := p.Chain.GenerateAccount()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		account := store.PoolAccount{Address: address, EncryptedSeed: seed, Ready: true}
		if err := p.db.Create(&account).Error; err != nil {
			t.Fatalf("seed pool: %v", err)
		}
	}
}

func orderBody(idempotencyKey string) string {
	return fmt.Sprintf(`{"idempotencyKey":%q,"amountNgn":10000,"bankCode":"033","bankAccount":"1234567890"}`, idempotencyKey)
}
// legacyOrderBody is what Linq-B2B sends: it fetches the rate and passes it on.func legacyOrderBody(idempotencyKey string) string {	return fmt.Sprintf(`{"idempotencyKey":%q,"amountNgn":10000,"rate":1655,"bankCode":"033","bankAccount":"1234567890"}`, idempotencyKey)}

// --- SD-1: keys ---

func TestAKeyIsShownOnceAndNeverStored(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "Acme")

	w := p.asAdmin(http.MethodPost, "/admin/integrators/"+integrator+"/keys", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	body := decode(t, w)
	full := body["key"].(string)
	if !strings.HasPrefix(full, "lq_live_"+body["keyPrefix"].(string)+"_") {
		t.Errorf("key %q does not carry its prefix", full)
	}
	secret := full[strings.LastIndex(full, "_")+1:]

	var row store.APIKey
	if err := p.db.First(&row, "id = ?", body["id"]).Error; err != nil {
		t.Fatalf("load key: %v", err)
	}
	if strings.Contains(row.SecretHash, secret) || strings.Contains(row.KeyPrefix, secret) {
		t.Error("the secret is stored in the clear")
	}

	// Nothing afterwards repeats it, and nothing logged it.
	events := p.asAdmin(http.MethodGet, "/admin/credential-events?integratorId="+integrator, "")
	if strings.Contains(events.Body.String(), secret) {
		t.Error("the credential events carry the secret")
	}
	if p.asKey(full, http.MethodGet, "/v1/orders/none", "").Code != http.StatusNotFound {
		t.Error("the issued key does not authenticate")
	}
	if strings.Contains(p.logs.String(), secret) {
		t.Error("the secret reached the log")
	}
	wantActions(t, p.actions(t, row.ID), store.CredentialIssued)
}

func TestRotationKeepsTheOldKeyForTheOverlap(t *testing.T) {
	p := newPublicAPI(t)
	oldID, oldKey := p.issue(t, p.integrator(t, "Acme"))

	w := p.asKey(oldKey, http.MethodPost, "/v1/keys/rotate", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	body := decode(t, w)
	newID, newKey := body["id"].(string), body["key"].(string)
	if newKey == oldKey {
		t.Fatal("rotation returned the same key")
	}
	start := p.clock

	use := func(key string) int { return p.asKey(key, http.MethodGet, "/v1/orders/none", "").Code }
	if got := use(oldKey); got != http.StatusNotFound {
		t.Errorf("old key straight after rotation: %d, want 404 (authenticated)", got)
	}
	if got := use(newKey); got != http.StatusNotFound {
		t.Errorf("new key straight after rotation: %d, want 404 (authenticated)", got)
	}

	p.clock = start.Add(store.RotationOverlap - time.Second)
	if got := use(oldKey); got != http.StatusNotFound {
		t.Errorf("old key at the end of the overlap: %d, want 404 (authenticated)", got)
	}

	p.clock = start.Add(store.RotationOverlap)
	if got := use(oldKey); got != http.StatusUnauthorized {
		t.Errorf("old key after the overlap: %d, want 401", got)
	}
	if got := use(newKey); got != http.StatusNotFound {
		t.Errorf("new key after the overlap: %d, want 404 (authenticated)", got)
	}

	wantActions(t, p.actions(t, oldID),
		store.CredentialIssued, store.CredentialRotated, store.CredentialExpired, store.CredentialRejectedExpired)
	wantActions(t, p.actions(t, newID), store.CredentialIssued)
}

// A leaked key inside its overlap must not be able to mint a successor that
// outlives the rotation.
func TestARotatedKeyCannotRotateAgain(t *testing.T) {
	p := newPublicAPI(t)
	_, oldKey := p.issue(t, p.integrator(t, "Acme"))

	if w := p.asKey(oldKey, http.MethodPost, "/v1/keys/rotate", ""); w.Code != http.StatusCreated {
		t.Fatalf("first rotation: %d", w.Code)
	}
	if w := p.asKey(oldKey, http.MethodPost, "/v1/keys/rotate", ""); w.Code != http.StatusConflict {
		t.Errorf("second rotation: %d, want 409", w.Code)
	}
	var keys int64
	p.db.Model(&store.APIKey{}).Count(&keys)
	if keys != 2 {
		t.Errorf("%d keys exist, want 2", keys)
	}
}

func TestAdminRotationAlsoOverlaps(t *testing.T) {
	p := newPublicAPI(t)
	oldID, oldKey := p.issue(t, p.integrator(t, "Acme"))

	w := p.asAdmin(http.MethodPost, "/admin/keys/"+oldID+"/rotate", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	if got := p.asKey(oldKey, http.MethodGet, "/v1/orders/none", "").Code; got != http.StatusNotFound {
		t.Errorf("old key after admin rotation: %d, want 404 (authenticated)", got)
	}
	var event store.CredentialEvent
	if err := p.db.First(&event, "key_id = ? AND action = ?", oldID, store.CredentialRotated).Error; err != nil {
		t.Fatalf("no rotation event: %v", err)
	}
	if event.Actor != store.ActorAdmin {
		t.Errorf("actor = %q, want admin", event.Actor)
	}
}

func TestRevocationIsImmediateAndRejectionsAreRecorded(t *testing.T) {
	p := newPublicAPI(t)
	id, key := p.issue(t, p.integrator(t, "Acme"))

	if w := p.asAdmin(http.MethodPost, "/admin/keys/"+id+"/revoke", ""); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	if w := p.asKey(key, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked key: %d, want 401", w.Code)
	}
	// Revoking twice is harmless and is not a second event.
	if w := p.asAdmin(http.MethodPost, "/admin/keys/"+id+"/revoke", ""); w.Code != http.StatusOK {
		t.Errorf("second revoke: %d, want 200", w.Code)
	}
	if w := p.asKey(key, http.MethodPost, "/v1/keys/rotate", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("rotating with a revoked key: %d, want 401", w.Code)
	}
	wantActions(t, p.actions(t, id),
		store.CredentialIssued, store.CredentialRevoked,
		store.CredentialRejectedRevoked, store.CredentialRejectedRevoked)
}

// Guessing wrong proves nothing about the key named, so it must not appear in
// the proof as a use of that key.
func TestAWrongSecretIsRefusedAndRecordsNothing(t *testing.T) {
	p := newPublicAPI(t)
	id, key := p.issue(t, p.integrator(t, "Acme"))

	last := len(key) - 1
	flipped := key[:last] + map[bool]string{true: "a", false: "b"}[key[last] != 'a']
	if w := p.asKey(flipped, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong secret: %d, want 401", w.Code)
	}
	wantActions(t, p.actions(t, id), store.CredentialIssued)
}

func TestSuspendedIntegratorIsRefused(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "Acme")
	_, key := p.issue(t, integrator)

	p.db.Model(&store.Integrator{}).Where("id = ?", integrator).Update("status", store.IntegratorSuspended)
	if w := p.asKey(key, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestAnotherIntegratorsOrderIs404(t *testing.T) {
	p := newPublicAPI(t)
	p.seedPool(t, 1)
	_, keyA := p.issue(t, p.integrator(t, "A"))
	_, keyB := p.issue(t, p.integrator(t, "B"))

	created := p.asKey(keyA, http.MethodPost, "/v1/orders", orderBody("same"))
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	path := "/v1/orders/" + decode(t, created)["id"].(string)

	if w := p.asKey(keyA, http.MethodGet, path, ""); w.Code != http.StatusOK {
		t.Errorf("owner: %d, want 200", w.Code)
	}
	if w := p.asKey(keyB, http.MethodGet, path, ""); w.Code != http.StatusNotFound {
		t.Errorf("other integrator: %d, want 404", w.Code)
	}
	// Linq-B2B's shared key is an integrator like any other.
	legacy := p.send(http.MethodGet, strings.TrimPrefix(path, "/v1"), "",
		map[string]string{"X-API-Key": p.OrdersAPIKey})
	if legacy.Code != http.StatusNotFound {
		t.Errorf("legacy key on a /v1 order: %d, want 404", legacy.Code)
	}
}

func TestIdempotencyIsPerIntegrator(t *testing.T) {
	p := newPublicAPI(t)
	p.seedPool(t, 3)
	_, keyA := p.issue(t, p.integrator(t, "A"))
	_, keyB := p.issue(t, p.integrator(t, "B"))

	first := p.asKey(keyA, http.MethodPost, "/v1/orders", orderBody("shared-key"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	repeat := p.asKey(keyA, http.MethodPost, "/v1/orders", orderBody("shared-key"))
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat: %d, want 200", repeat.Code)
	}
	if decode(t, repeat)["id"] != decode(t, first)["id"] {
		t.Error("a repeated key from one integrator made a second order")
	}

	other := p.asKey(keyB, http.MethodPost, "/v1/orders", orderBody("shared-key"))
	if other.Code != http.StatusCreated {
		t.Fatalf("other integrator: %d %s", other.Code, other.Body.String())
	}
	if decode(t, other)["id"] == decode(t, first)["id"] {
		t.Error("one integrator was handed another's order")
	}

	legacy := p.send(http.MethodPost, "/orders", legacyOrderBody("shared-key"),
		map[string]string{"X-API-Key": p.OrdersAPIKey})
	if legacy.Code != http.StatusCreated {
		t.Fatalf("legacy: %d %s", legacy.Code, legacy.Body.String())
	}
	var order store.Order
	if err := p.db.First(&order, "id = ?", decode(t, legacy)["id"]).Error; err != nil {
		t.Fatalf("load order: %v", err)
	}
	if order.IntegratorID != store.LegacyIntegratorID {
		t.Errorf("legacy order belongs to %q", order.IntegratorID)
	}
}

// /v1 serves the same body as the legacy route, not a lookalike.
func TestV1OrderMatchesTheLegacyResponse(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "A")
	_, key := p.issue(t, integrator)
	p.db.Create(&store.Order{
		ID: "shared", IdempotencyKey: "k", IntegratorID: integrator, Status: store.StateAwaitingDeposit,
		DepositAddress: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H", AmountUSDC: 5, Rate: 1655,
	})
	p.db.Create(&store.Order{
		ID: "legacy", IdempotencyKey: "k", Status: store.StateAwaitingDeposit,
		DepositAddress: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H", AmountUSDC: 5, Rate: 1655,
	})

	v1 := decode(t, p.asKey(key, http.MethodGet, "/v1/orders/shared", ""))
	legacy := decode(t, p.send(http.MethodGet, "/orders/legacy", "", map[string]string{"X-API-Key": p.OrdersAPIKey}))
	for name := range legacy {
		if _, ok := v1[name]; !ok {
			t.Errorf("/v1 is missing %q", name)
		}
	}
	for name := range v1 {
		// payoutAttempts is /v1 only (SD-4); the legacy body stays as it was.
		if _, ok := legacy[name]; !ok && name != "payoutAttempts" {
			t.Errorf("/v1 adds %q", name)
		}
	}
	if _, ok := legacy["payoutAttempts"]; ok {
		t.Errorf("the legacy response gained payoutAttempts")
	}
}

// The two credentials do not cross: the shared key has no row to be limited or
// rotated by, and an issued key is not the shared secret.
func TestLegacyAndIssuedKeysDoNotCross(t *testing.T) {
	p := newPublicAPI(t)
	_, key := p.issue(t, p.integrator(t, "A"))

	if w := p.asKey(p.OrdersAPIKey, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("shared key on /v1: %d, want 401", w.Code)
	}
	if w := p.asKey(key, http.MethodGet, "/orders/none", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("issued key on /orders: %d, want 401", w.Code)
	}
}

func TestAKeyFromTheOtherNetworkIsRefused(t *testing.T) {
	p := newPublicAPI(t)
	_, key := p.issue(t, p.integrator(t, "A"))
	test := strings.Replace(key, "lq_live_", "lq_test_", 1)
	if w := p.asKey(test, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAdminRoutesRequireTheAdminKey(t *testing.T) {
	p := newPublicAPI(t)
	id, key := p.issue(t, p.integrator(t, "A"))

	paths := []struct{ method, path string }{
		{http.MethodPost, "/admin/integrators"},
		{http.MethodPut, "/admin/integrators/x/rate-limit"},
		{http.MethodPost, "/admin/integrators/x/keys"},
		{http.MethodPost, "/admin/keys/" + id + "/rotate"},
		{http.MethodPost, "/admin/keys/" + id + "/revoke"},
		{http.MethodGet, "/admin/credential-events"},
	}
	for _, route := range paths {
		for name, headers := range map[string]map[string]string{
			"no key":                  nil,
			"wrong key":               {"X-Admin-Key": "wrong"},
			"integrator key":          {"X-Admin-Key": key},
			"key in the wrong header": {"X-API-Key": testAdminKey},
		} {
			if w := p.send(route.method, route.path, "{}", headers); w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d, want 401", route.method, route.path, name, w.Code)
			}
		}
	}

	// An unset admin key must match nothing, including an empty header.
	p.AdminAPIKey = ""
	if w := p.send(http.MethodGet, "/admin/credential-events", "", map[string]string{"X-Admin-Key": ""}); w.Code != http.StatusUnauthorized {
		t.Errorf("unset admin key: %d, want 401", w.Code)
	}
}

func TestAdminErrors(t *testing.T) {
	p := newPublicAPI(t)
	cases := []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/admin/integrators", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/admin/integrators", `{"name":"x","rateLimitPerMinute":0}`, http.StatusBadRequest},
		{http.MethodPost, "/admin/integrators/nope/keys", "", http.StatusNotFound},
		{http.MethodPut, "/admin/integrators/nope/rate-limit", `{"rateLimitPerMinute":5}`, http.StatusNotFound},
		{http.MethodPost, "/admin/keys/nope/rotate", "", http.StatusNotFound},
		{http.MethodPost, "/admin/keys/nope/revoke", "", http.StatusNotFound},
		{http.MethodGet, "/admin/credential-events?limit=0", "", http.StatusBadRequest},
	}
	for _, c := range cases {
		if w := p.asAdmin(c.method, c.path, c.body); w.Code != c.want {
			t.Errorf("%s %s: %d, want %d (%s)", c.method, c.path, w.Code, c.want, w.Body.String())
		}
	}
}

func TestCredentialEventsCanBeFiltered(t *testing.T) {
	p := newPublicAPI(t)
	integratorA := p.integrator(t, "A")
	idA, _ := p.issue(t, integratorA)
	p.issue(t, integratorA)
	p.issue(t, p.integrator(t, "B"))

	count := func(query string) int {
		w := p.asAdmin(http.MethodGet, "/admin/credential-events"+query, "")
		if w.Code != http.StatusOK {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		return len(decode(t, w)["events"].([]any))
	}
	for query, want := range map[string]int{
		"":                             3,
		"?integratorId=" + integratorA: 2,
		"?keyId=" + idA:                1,
		"?limit=1":                     1,
		"?integratorId=int_nobody":     0,
	} {
		if got := count(query); got != want {
			t.Errorf("%q: %d events, want %d", query, got, want)
		}
	}
}

// PUBLIC_API_ENABLED is the off switch: with it unset /v1 and /admin do not
// exist, whatever keys were issued, and the legacy routes do not notice.
func TestPublicAPIIsOffUnlessEnabled(t *testing.T) {
	p := newPublicAPI(t)
	_, key := p.issue(t, p.integrator(t, "A"))
	p.PublicAPIEnabled = false

	if w := p.asKey(key, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Errorf("/v1 with the switch off: %d", w.Code)
	} else if strings.Contains(w.Body.String(), "order not found") {
		t.Error("/v1 reached the order handler with the switch off")
	}
	if w := p.asAdmin(http.MethodPost, "/admin/integrators", `{"name":"x"}`); w.Code == http.StatusCreated {
		t.Error("/admin served with the switch off")
	}
	legacy := p.send(http.MethodGet, "/orders/none", "", map[string]string{"X-API-Key": p.OrdersAPIKey})
	if legacy.Code != http.StatusNotFound || !strings.Contains(legacy.Body.String(), "order not found") {
		t.Errorf("legacy route: %d %s", legacy.Code, legacy.Body.String())
	}
}

// --- SD-2: rate limits ---

func (p *publicAPI) setLimit(t *testing.T, integrator string, limit int) {
	t.Helper()
	w := p.asAdmin(http.MethodPut, "/admin/integrators/"+integrator+"/rate-limit",
		fmt.Sprintf(`{"rateLimitPerMinute":%d}`, limit))
	if w.Code != http.StatusOK {
		t.Fatalf("set limit: %d %s", w.Code, w.Body.String())
	}
}

func header(t *testing.T, w *httptest.ResponseRecorder, name string) int {
	t.Helper()
	n, err := strconv.Atoi(w.Header().Get(name))
	if err != nil {
		t.Fatalf("%s = %q: %v", name, w.Header().Get(name), err)
	}
	return n
}

func TestTheLimitRefusesWithRetryAfterAndLogsIt(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "A")
	p.setLimit(t, integrator, 3)
	id, key := p.issue(t, integrator)

	for i := 1; i <= 3; i++ {
		w := p.asKey(key, http.MethodGet, "/v1/orders/none", "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("request %d: %d, want 404 (let through)", i, w.Code)
		}
		if got := header(t, w, "X-RateLimit-Limit"); got != 3 {
			t.Errorf("request %d: limit header = %d", i, got)
		}
		if got := header(t, w, "X-RateLimit-Remaining"); got != 3-i {
			t.Errorf("request %d: remaining = %d, want %d", i, got, 3-i)
		}
		if w.Header().Get("Retry-After") != "" {
			t.Errorf("request %d: Retry-After on an allowed request", i)
		}
	}
	if strings.Contains(p.logs.String(), "rate limit refused") {
		t.Fatal("logged a refusal before there was one")
	}

	w := p.asKey(key, http.MethodGet, "/v1/orders/none", "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d, want 429", w.Code)
	}
	if got := header(t, w, "X-RateLimit-Limit"); got != 3 {
		t.Errorf("limit header on the 429 = %d", got)
	}
	if got := header(t, w, "X-RateLimit-Remaining"); got != 0 {
		t.Errorf("remaining on the 429 = %d", got)
	}
	// The clock is 15s into the minute.
	if got := header(t, w, "Retry-After"); got != 45 {
		t.Errorf("Retry-After = %d, want 45", got)
	}

	logged := p.logs.String()
	for _, want := range []string{"rate limit refused", "key=" + id, "integrator=" + integrator, "at=2026-11-08T12:00:15Z"} {
		if !strings.Contains(logged, want) {
			t.Errorf("refusal log is missing %q:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, key[strings.LastIndex(key, "_")+1:]) {
		t.Error("the secret reached the log")
	}

	// A second refusal in the same window is its own line.
	p.asKey(key, http.MethodGet, "/v1/orders/none", "")
	if got := strings.Count(p.logs.String(), "rate limit refused"); got != 2 {
		t.Errorf("%d refusal lines, want 2", got)
	}
}

// A refused create must stop before anything is provisioned.
func TestARefusedCreateProvisionsNothing(t *testing.T) {
	p := newPublicAPI(t)
	p.seedPool(t, 2)
	integrator := p.integrator(t, "A")
	p.setLimit(t, integrator, 1)
	_, key := p.issue(t, integrator)

	if w := p.asKey(key, http.MethodPost, "/v1/orders", orderBody("one")); w.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	if w := p.asKey(key, http.MethodPost, "/v1/orders", orderBody("two")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second: %d, want 429", w.Code)
	}
	var orders, claimed int64
	p.db.Model(&store.Order{}).Count(&orders)
	p.db.Model(&store.PoolAccount{}).Where("claimed_at IS NOT NULL").Count(&claimed)
	if orders != 1 || claimed != 1 {
		t.Errorf("%d orders and %d claimed accounts, want 1 and 1", orders, claimed)
	}
}

func TestTheDefaultIs120AndAnOverrideReplacesIt(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "A")
	_, key := p.issue(t, integrator)

	remaining := func() (limit, left int) {
		w := p.asKey(key, http.MethodGet, "/v1/orders/none", "")
		return header(t, w, "X-RateLimit-Limit"), header(t, w, "X-RateLimit-Remaining")
	}
	if limit, left := remaining(); limit != 120 || left != 119 {
		t.Errorf("default: limit %d remaining %d, want 120 and 119", limit, left)
	}
	p.setLimit(t, integrator, 500)
	if limit, left := remaining(); limit != 500 || left != 498 {
		t.Errorf("override: limit %d remaining %d, want 500 and 498", limit, left)
	}
	// Clearing the override goes back to the default.
	w := p.asAdmin(http.MethodPut, "/admin/integrators/"+integrator+"/rate-limit", `{"rateLimitPerMinute":null}`)
	if w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	if limit, left := remaining(); limit != 120 || left != 117 {
		t.Errorf("cleared: limit %d remaining %d, want 120 and 117", limit, left)
	}
}

func TestTheLimitResetsInTheNextMinute(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "A")
	p.setLimit(t, integrator, 2)
	_, key := p.issue(t, integrator)

	for i := 0; i < 2; i++ {
		p.asKey(key, http.MethodGet, "/v1/orders/none", "")
	}
	if w := p.asKey(key, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d, want 429", w.Code)
	}

	// The last second of the window is still the same window.
	p.clock = time.Date(2026, 11, 8, 12, 0, 59, 500_000_000, time.UTC)
	w := p.asKey(key, http.MethodGet, "/v1/orders/none", "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("end of window: %d, want 429", w.Code)
	}
	if got := header(t, w, "Retry-After"); got != 1 {
		t.Errorf("Retry-After = %d, want 1 (rounded up)", got)
	}

	p.clock = time.Date(2026, 11, 8, 12, 1, 0, 0, time.UTC)
	w = p.asKey(key, http.MethodGet, "/v1/orders/none", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("after the reset: %d, want 404 (let through)", w.Code)
	}
	if got := header(t, w, "X-RateLimit-Remaining"); got != 1 {
		t.Errorf("remaining after the reset = %d, want 1", got)
	}
}

func TestTwoKeysCountSeparately(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "A")
	p.setLimit(t, integrator, 2)
	_, first := p.issue(t, integrator)
	_, second := p.issue(t, integrator)

	for i := 0; i < 3; i++ {
		p.asKey(first, http.MethodGet, "/v1/orders/none", "")
	}
	if w := p.asKey(first, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("first key: %d, want 429", w.Code)
	}
	w := p.asKey(second, http.MethodGet, "/v1/orders/none", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("second key: %d, want 404 (let through)", w.Code)
	}
	if got := header(t, w, "X-RateLimit-Remaining"); got != 1 {
		t.Errorf("second key remaining = %d, want 1", got)
	}
}

// Only someone holding a key can spend it. A stranger who has seen a prefix must
// not be able to lock the owner out.
func TestFailedAuthDoesNotSpendTheAllowance(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "A")
	p.setLimit(t, integrator, 1)
	_, key := p.issue(t, integrator)

	last := len(key) - 1
	wrong := key[:last] + map[bool]string{true: "a", false: "b"}[key[last] != 'a']
	for i := 0; i < 5; i++ {
		w := p.asKey(wrong, http.MethodGet, "/v1/orders/none", "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong secret: %d, want 401", w.Code)
		}
		if w.Header().Get("X-RateLimit-Limit") != "" {
			t.Error("rate limit headers on a request that named no key")
		}
	}
	if w := p.asKey(key, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusNotFound {
		t.Errorf("owner: %d, want 404 (let through)", w.Code)
	}
}

func TestTheLimitIgnoresTheCallersAddress(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "A")
	p.setLimit(t, integrator, 2)
	_, first := p.issue(t, integrator)
	_, second := p.issue(t, integrator)

	from := func(key, addr string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/orders/none", nil)
		r.RemoteAddr = addr
		r.Header.Set("X-Forwarded-For", addr)
		r.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		p.Routes().ServeHTTP(w, r)
		return w.Code
	}

	// Moving address does not get a key a fresh allowance.
	from(first, "10.0.0.1:1000")
	from(first, "10.0.0.2:1000")
	if got := from(first, "10.0.0.3:1000"); got != http.StatusTooManyRequests {
		t.Errorf("same key, new address: %d, want 429", got)
	}
	// And sharing one address does not share a limit.
	if got := from(second, "10.0.0.1:1000"); got != http.StatusNotFound {
		t.Errorf("other key, same address: %d, want 404 (let through)", got)
	}
}

// The count lives in the database, so a second instance sees the first's.
func TestTheLimitHoldsAcrossServerInstances(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "A")
	p.setLimit(t, integrator, 2)
	_, key := p.issue(t, integrator)

	copied := *p.Server
	other := &publicAPI{Server: &copied, db: p.db, clock: p.clock, logs: p.logs}

	p.asKey(key, http.MethodGet, "/v1/orders/none", "")
	other.asKey(key, http.MethodGet, "/v1/orders/none", "")
	if w := p.asKey(key, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", w.Code)
	}
}

func TestTheLegacyRoutesCarryNoRateLimit(t *testing.T) {
	p := newPublicAPI(t)
	w := p.send(http.MethodGet, "/orders/none", "", map[string]string{"X-API-Key": p.OrdersAPIKey})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if w.Header().Get("X-RateLimit-Limit") != "" {
		t.Error("the legacy route grew a rate limit header")
	}
	var counters int64
	p.db.Model(&store.RateCounter{}).Count(&counters)
	if counters != 0 {
		t.Errorf("%d counters written for a legacy request", counters)
	}
}

// legacyOrderBody is what Linq-B2B sends: it fetches the rate and passes it on.
func legacyOrderBody(idempotencyKey string) string {
	return fmt.Sprintf(`{"idempotencyKey":%q,"amountNgn":10000,"rate":1655,"bankCode":"033","bankAccount":"1234567890"}`, idempotencyKey)
}

// --- SD-4: payout attempts and induced failures ---

func TestV1ListsEveryPayoutReference(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "A")
	_, key := p.issue(t, integrator)
	p.db.Create(&store.Order{ID: "ord_1", IdempotencyKey: "k", IntegratorID: integrator, Status: store.StatePayoutQueued})
	if _, err := store.RecordInducedFailure(p.db, "ord_1", 1, "ord_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartPayoutAttempt(p.db, "ord_1", 2, "ord_1-p2"); err != nil {
		t.Fatal(err)
	}

	body := decode(t, p.asKey(key, http.MethodGet, "/v1/orders/ord_1", ""))
	attempts, _ := body["payoutAttempts"].([]any)
	if len(attempts) != 2 {
		t.Fatalf("payoutAttempts = %v, want two", body["payoutAttempts"])
	}
	first, second := attempts[0].(map[string]any), attempts[1].(map[string]any)
	if first["reference"] != "ord_1" || first["outcome"] != "failed" || first["induced"] != true ||
		second["reference"] != "ord_1-p2" || second["outcome"] != "pending" || second["induced"] != false {
		t.Errorf("payoutAttempts = %v", attempts)
	}
	if _, ok := first["reason"]; ok {
		t.Errorf("an attempt's internal reason was served: %v", first)
	}
}

func TestInducingAPayoutFailureNeedsTheIntegratorFlag(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "Linq test")
	p.db.Create(&store.Order{ID: "ord_1", IdempotencyKey: "k", IntegratorID: integrator, Status: store.StateAwaitingDeposit})
	path := "/admin/orders/ord_1/induce-payout-failure"

	if w := p.asAdmin(http.MethodPost, path, `{"kind":"terminal"}`); w.Code != http.StatusForbidden {
		t.Errorf("without the flag: %d %s, want 403", w.Code, w.Body.String())
	}

	p.db.Model(&store.Integrator{}).Where("id = ?", integrator).Update("allow_failure_injection", true)
	if w := p.asAdmin(http.MethodPost, path, `{"kind":"sometimes"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad kind: %d, want 400", w.Code)
	}
	if w := p.asAdmin(http.MethodPost, "/admin/orders/nope/induce-payout-failure", `{"kind":"terminal"}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown order: %d, want 404", w.Code)
	}
	if w := p.asAdmin(http.MethodPost, path, `{"kind":"terminal"}`); w.Code != http.StatusOK {
		t.Errorf("with the flag: %d %s, want 200", w.Code, w.Body.String())
	}
	if w := p.asAdmin(http.MethodPost, path, `{"kind":"transient"}`); w.Code != http.StatusConflict {
		t.Errorf("second time: %d, want 409", w.Code)
	}
	if w := p.send(http.MethodPost, path, `{"kind":"terminal"}`, nil); w.Code == http.StatusOK {
		t.Errorf("without the admin key: %d, want a refusal", w.Code)
	}

	kind, err := store.InducedFailureOf(p.db, "ord_1")
	if err != nil || kind != store.InducedTerminal {
		t.Errorf("induced failure = %q, %v; want terminal", kind, err)
	}
}
