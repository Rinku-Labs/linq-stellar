package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

func (p *publicAPI) setWebhook(t *testing.T, integrator, body string) (int, map[string]any) {
	t.Helper()
	w := p.asAdmin(http.MethodPut, "/admin/integrators/"+integrator+"/webhook", body)
	out := map[string]any{"cache": w.Header().Get("Cache-Control")}
	if strings.HasPrefix(w.Body.String(), "{") {
		for k, v := range decode(t, w) {
			out[k] = v
		}
	}
	return w.Code, out
}

func TestSettingAWebhookMintsASecretShownOnce(t *testing.T) {
	p := newPublicAPI(t)
	id := p.integrator(t, "A")

	code, body := p.setWebhook(t, id, `{"url":"https://hooks.example.com/linq"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, body)
	}
	if body["cache"] != "no-store" {
		t.Errorf("Cache-Control = %v", body["cache"])
	}
	secret, _ := body["webhookSecret"].(string)
	if !strings.HasPrefix(secret, "whsec_") || len(secret) < 40 {
		t.Fatalf("secret = %q", secret)
	}

	var row store.Integrator
	if err := p.db.First(&row, "id = ?", id).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if row.WebhookURL != "https://hooks.example.com/linq" {
		t.Errorf("url = %q", row.WebhookURL)
	}
	if strings.Contains(row.WebhookSecretEncrypted, secret) || row.WebhookSecretEncrypted == "" {
		t.Error("the secret is not stored sealed")
	}
	opened, err := p.Chain.DecryptSeed(row.WebhookSecretEncrypted)
	if err != nil || opened != secret {
		t.Errorf("the worker could not open it: %v", err)
	}
	if strings.Contains(p.logs.String(), secret) {
		t.Error("the secret was logged")
	}

}

// Moving an endpoint must not break the receiver's signature check.
func TestMovingAWebhookKeepsItsSecret(t *testing.T) {
	p := newPublicAPI(t)
	id := p.integrator(t, "A")
	_, first := p.setWebhook(t, id, `{"url":"https://hooks.example.com/linq"}`)
	var before store.Integrator
	p.db.First(&before, "id = ?", id)

	code, moved := p.setWebhook(t, id, `{"url":"https://new.example.com/linq"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, moved)
	}
	if _, shown := moved["webhookSecret"]; shown {
		t.Error("the secret was shown a second time")
	}
	var after store.Integrator
	p.db.First(&after, "id = ?", id)
	if after.WebhookURL != "https://new.example.com/linq" {
		t.Errorf("url = %q", after.WebhookURL)
	}
	if after.WebhookSecretEncrypted != before.WebhookSecretEncrypted {
		t.Error("moving the URL changed the secret")
	}

	// A leaked secret is replaced in place: the webhook is never empty, so there
	// is no moment for a pass to find nothing to send to.
	code, replaced := p.setWebhook(t, id, `{"url":"https://new.example.com/linq","rotateSecret":true}`)
	if code != http.StatusOK || replaced["webhookSecret"] == nil || replaced["webhookSecret"] == first["webhookSecret"] {
		t.Errorf("rotate: %d, secret = %v", code, replaced["webhookSecret"])
	}
	var rotated store.Integrator
	p.db.First(&rotated, "id = ?", id)
	if rotated.WebhookSecretEncrypted == before.WebhookSecretEncrypted || rotated.WebhookURL == "" {
		t.Errorf("after rotating: %+v", rotated)
	}
	if strings.Contains(p.logs.String(), replaced["webhookSecret"].(string)) {
		t.Error("the secret was logged")
	}

	if code, _ := p.setWebhook(t, id, `{"url":"","rotateSecret":true}`); code != http.StatusBadRequest {
		t.Errorf("rotating with no url: %d, want 400", code)
	}
}

// A refused URL must not touch what is already working.
func TestABadURLLeavesTheWebhookAlone(t *testing.T) {
	p := newPublicAPI(t)
	id := p.integrator(t, "A")
	p.setWebhook(t, id, `{"url":"https://hooks.example.com/linq"}`)
	if code, _ := p.setWebhook(t, id, `{"url":"http://169.254.169.254/"}`); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	var row store.Integrator
	p.db.First(&row, "id = ?", id)
	if row.WebhookURL != "https://hooks.example.com/linq" {
		t.Errorf("url = %q", row.WebhookURL)
	}
}

func TestAWebhookCanBeCleared(t *testing.T) {
	p := newPublicAPI(t)
	id := p.integrator(t, "A")
	p.setWebhook(t, id, `{"url":"https://hooks.example.com/linq"}`)

	if code, body := p.setWebhook(t, id, `{"url":""}`); code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, body)
	}
	var row store.Integrator
	p.db.First(&row, "id = ?", id)
	if row.WebhookURL != "" || row.WebhookSecretEncrypted != "" {
		t.Errorf("row = %+v", row)
	}
}

func TestWebhookURLsAreChecked(t *testing.T) {
	p := newPublicAPI(t)
	id := p.integrator(t, "A")
	for _, url := range []string{
		"http://hooks.example.com",
		"https://169.254.169.254/latest/meta-data/",
		"https://127.0.0.1/hook",
		"https://localhost/hook",
	} {
		if code, _ := p.setWebhook(t, id, `{"url":"`+url+`"}`); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", url, code)
		}
	}
	var row store.Integrator
	p.db.First(&row, "id = ?", id)
	if row.WebhookURL != "" {
		t.Errorf("stored %q", row.WebhookURL)
	}
}

// Its webhook comes from the environment, and a row here would be silently
// ignored.
func TestLinqB2BsWebhookCannotBeSetHere(t *testing.T) {
	p := newPublicAPI(t)
	if code, _ := p.setWebhook(t, store.LegacyIntegratorID, `{"url":"https://hooks.example.com"}`); code != http.StatusConflict {
		t.Errorf("status = %d, want 409", code)
	}
}

func TestSettingAWebhookNeedsAnExistingIntegrator(t *testing.T) {
	p := newPublicAPI(t)
	if code, _ := p.setWebhook(t, "nope", `{"url":"https://hooks.example.com"}`); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

// An integrator's key can't use the admin route, for its own webhook or anyone
// else's. It sets its own through PUT /v1/webhook.
func TestOnlyTheAdminKeySetsWebhooks(t *testing.T) {
	p := newPublicAPI(t)
	id := p.integrator(t, "A")
	_, key := p.issue(t, id)
	for name, w := range map[string]int{
		"no key":         p.send(http.MethodPut, "/admin/integrators/"+id+"/webhook", `{"url":"https://x.example"}`, nil).Code,
		"integrator key": p.send(http.MethodPut, "/admin/integrators/"+id+"/webhook", `{"url":"https://x.example"}`, map[string]string{"X-Admin-Key": key}).Code,
	} {
		if w != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, w)
		}
	}
}

func (p *publicAPI) setOwnWebhook(t *testing.T, key, body string) (int, map[string]any) {
	t.Helper()
	w := p.asKey(key, http.MethodPut, "/v1/webhook", body)
	out := map[string]any{"cache": w.Header().Get("Cache-Control")}
	if strings.HasPrefix(w.Body.String(), "{") {
		for k, v := range decode(t, w) {
			out[k] = v
		}
	}
	return w.Code, out
}

// A partner, or a reviewer on the test deployment, points its own webhooks
// somewhere with its own key, under the admin route's rules.
func TestAnIntegratorSetsItsOwnWebhook(t *testing.T) {
	p := newPublicAPI(t)
	id := p.integrator(t, "Reviewer")
	_, key := p.issue(t, id)
	schema := loadSpec(t).Components.Schemas["Webhook"]
	stored := func() store.Integrator {
		var row store.Integrator
		p.db.First(&row, "id = ?", id)
		return row
	}

	code, set := p.setOwnWebhook(t, key, `{"url":"https://hooks.example.com/linq"}`)
	secret, _ := set["webhookSecret"].(string)
	if code != http.StatusOK || !strings.HasPrefix(secret, "whsec_") || set["cache"] != "no-store" || set["id"] != id {
		t.Fatalf("set: %d %v", code, set)
	}
	delete(set, "cache")
	assertMatchesSchema(t, schema, set)
	if row := stored(); row.WebhookURL != "https://hooks.example.com/linq" {
		t.Errorf("url = %q", row.WebhookURL)
	}
	if strings.Contains(p.logs.String(), secret) {
		t.Error("the secret was logged")
	}

	// Moving keeps the secret, and says nothing of it.
	sealed := stored().WebhookSecretEncrypted
	if code, moved := p.setOwnWebhook(t, key, `{"url":"https://new.example.com/linq"}`); code != http.StatusOK || moved["webhookSecret"] != nil {
		t.Errorf("move: %d %v", code, moved)
	}
	if row := stored(); row.WebhookURL != "https://new.example.com/linq" || row.WebhookSecretEncrypted != sealed {
		t.Errorf("after the move: %q, secret kept = %v", row.WebhookURL, row.WebhookSecretEncrypted == sealed)
	}

	// Rotating mints a new one.
	if code, rotated := p.setOwnWebhook(t, key, `{"url":"https://new.example.com/linq","rotateSecret":true}`); code != http.StatusOK || rotated["webhookSecret"] == nil || rotated["webhookSecret"] == secret {
		t.Errorf("rotate: %d %v", code, rotated)
	}

	// Clearing stops deliveries.
	code, cleared := p.setOwnWebhook(t, key, `{"url":""}`)
	if code != http.StatusOK || cleared["webhookUrl"] != nil || stored().WebhookURL != "" {
		t.Errorf("clear: %d %v", code, cleared)
	}
	delete(cleared, "cache")
	assertMatchesSchema(t, schema, cleared)
}

// The key reaches its own integrator's webhook and nobody else's, and the same
// URL checks apply.
func TestAnIntegratorsKeyOnlyReachesItsOwnWebhook(t *testing.T) {
	p := newPublicAPI(t)
	mine := p.integrator(t, "Mine")
	theirs := p.integrator(t, "Theirs")
	_, key := p.issue(t, mine)
	p.setWebhook(t, theirs, `{"url":"https://theirs.example.com/hook"}`)

	if code, _ := p.setOwnWebhook(t, key, `{"url":"https://mine.example.com/hook","id":"`+theirs+`"}`); code != http.StatusOK {
		t.Fatalf("set: %d", code)
	}
	var row store.Integrator
	p.db.First(&row, "id = ?", theirs)
	if row.WebhookURL != "https://theirs.example.com/hook" {
		t.Errorf("another integrator's webhook moved to %q", row.WebhookURL)
	}

	for _, url := range []string{"http://hooks.example.com", "https://169.254.169.254/latest/meta-data/", "https://127.0.0.1/hook"} {
		if code, _ := p.setOwnWebhook(t, key, `{"url":"`+url+`"}`); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", url, code)
		}
	}
	for name, headers := range map[string]map[string]string{
		"no key":        nil,
		"the admin key": {"X-API-Key": p.AdminAPIKey},
	} {
		if w := p.send(http.MethodPut, "/v1/webhook", `{"url":"https://x.example.com"}`, headers); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, w.Code)
		}
	}
}
