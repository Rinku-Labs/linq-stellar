package api

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Rinku-Labs/linq-stellar/docs"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"gopkg.in/yaml.v3"
)

// These tests hold the docs to the code: a route, field or status added on one
// side and not the other fails here.

type specSchema struct {
	Required   []string                  `yaml:"required"`
	Properties map[string]map[string]any `yaml:"properties"`
	Enum       []string                  `yaml:"enum"`
}

type spec struct {
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas map[string]specSchema `yaml:"schemas"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T) spec {
	t.Helper()
	var s spec
	if err := yaml.Unmarshal(docs.OpenAPI, &s); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return s
}

func TestTheSpecCoversEveryV1Route(t *testing.T) {
	source, err := os.ReadFile("public.go")
	if err != nil {
		t.Fatalf("read routes: %v", err)
	}
	var code []string
	for _, match := range regexp.MustCompile(`HandleFunc\("(\w+) (/v1/[^"]+)"`).FindAllStringSubmatch(string(source), -1) {
		code = append(code, match[1]+" "+match[2])
	}

	var documented []string
	for path, operations := range loadSpec(t).Paths {
		for method := range operations {
			documented = append(documented, strings.ToUpper(method)+" "+path)
		}
	}
	slices.Sort(code)
	slices.Sort(documented)
	if len(code) == 0 || !slices.Equal(code, documented) {
		t.Errorf("routes in code = %v\nroutes in spec = %v", code, documented)
	}
}

func TestEveryStatusIsDocumented(t *testing.T) {
	want := []string{
		store.StateInitiated, store.StateAwaitingDeposit, store.StateDepositDetected,
		store.StatePayoutQueued, store.StatePayoutProcessing, store.StateDisbursed,
		store.StateSweepQueued, store.StateSweepProcessing, store.StateSettledInTreasury,
		store.StateRefundQueued, store.StateRefundProcessing, store.StateRefunded,
		store.StateExpired, store.StateFailed,
	}
	enum := loadSpec(t).Components.Schemas["OrderStatus"].Enum
	guide := string(docs.Guide)
	for _, status := range want {
		if !slices.Contains(enum, status) {
			t.Errorf("%s is not in the spec", status)
		}
		if !strings.Contains(guide, "| `"+status+"` |") {
			t.Errorf("%s is not in the guide's table", status)
		}
		if final := finalInGuide(guide, status); final != store.IsTerminal(status) {
			t.Errorf("%s: guide says final = %v", status, final)
		}
	}
	if len(enum) != len(want) {
		t.Errorf("spec lists %d statuses, want %d", len(enum), len(want))
	}
}

func finalInGuide(guide, status string) bool {
	for _, line := range strings.Split(guide, "\n") {
		if strings.HasPrefix(line, "| `"+status+"` |") {
			return strings.HasSuffix(strings.TrimSpace(line), "| Yes |")
		}
	}
	return false
}

// Every field a real reply carries must be documented, and every required field
// must really be there.
func assertMatchesSchema(t *testing.T, s specSchema, body map[string]any) {
	t.Helper()
	for field := range body {
		if _, ok := s.Properties[field]; !ok {
			t.Errorf("reply has undocumented field %q", field)
		}
	}
	for _, field := range s.Required {
		if _, ok := body[field]; !ok {
			t.Errorf("reply is missing required field %q", field)
		}
	}
}

func TestRepliesMatchTheSpec(t *testing.T) {
	schemas := loadSpec(t).Components.Schemas
	p := newPublicAPI(t)
	p.seedPool(t, 1)
	integrator := p.integrator(t, "Partner")
	_, key := p.issue(t, integrator)

	created := p.asKey(key, http.MethodPost, "/v1/orders", orderBody("spec"))
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	order := decode(t, created)
	assertMatchesSchema(t, schemas["Order"], order)

	// Every optional field too, so the check above is not only the happy path.
	full := store.Order{
		ID: "full", IdempotencyKey: "full", IntegratorID: integrator,
		Status: store.StateRefunded, DepositAddress: "G", DepositTxHash: "d", SweepTxHash: "s",
		PayoutReference: "p", RefundTxHash: "r", RefundAddress: "G2", Underpaid: true, ShortfallNGN: 1,
	}
	if err := p.db.Create(&full).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Delivered, so the fields a reported move carries are checked too.
	notifiedAt := p.clock
	event := store.StatusEvent{OrderID: "full", Kind: store.KindTransition, To: store.StateRefunded, Reason: "why", At: p.clock, NotifiedAt: &notifiedAt}
	if err := p.db.Create(&event).Error; err != nil {
		t.Fatalf("seed event: %v", err)
	}
	read := p.asKey(key, http.MethodGet, "/v1/orders/full", "")
	if read.Code != http.StatusOK {
		t.Fatalf("read: %d %s", read.Code, read.Body.String())
	}
	fullBody := decode(t, read)
	assertMatchesSchema(t, schemas["Order"], fullBody)
	for _, entry := range fullBody["statusHistory"].([]any) {
		assertMatchesSchema(t, schemas["StatusEvent"], entry.(map[string]any))
	}

	rotated := p.asKey(key, http.MethodPost, "/v1/keys/rotate", "")
	if rotated.Code != http.StatusCreated {
		t.Fatalf("rotate: %d %s", rotated.Code, rotated.Body.String())
	}
	assertMatchesSchema(t, schemas["RotatedKey"], decode(t, rotated))
}

// Every field the handler reads must be in the spec, and the reverse, or an
// integrator is told about a field that does nothing.
func TestTheRequestSchemaMatchesTheHandler(t *testing.T) {
	raw, err := json.Marshal(CreateOrderRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	delete(fields, "rate") // accepted only from the legacy caller

	documented := loadSpec(t).Components.Schemas["CreateOrderRequest"].Properties
	for field := range fields {
		if _, ok := documented[field]; !ok {
			t.Errorf("%s is not documented", field)
		}
	}
	for field := range documented {
		if _, ok := fields[field]; !ok {
			t.Errorf("%s is documented but not read", field)
		}
	}
}

func TestTheDocsAreServed(t *testing.T) {
	p := newPublicAPI(t)
	for path, want := range map[string]struct{ contentType, body string }{
		"/docs":              {"text/html; charset=utf-8", "<h2>Rate limits</h2>"},
		"/docs/":             {"text/html; charset=utf-8", "<h2>Rate limits</h2>"},
		"/docs/README.md":    {"text/markdown; charset=utf-8", "## Rate limits"},
		"/docs/openapi.yaml": {"application/yaml; charset=utf-8", "openapi: 3.0.3"},
	} {
		w := p.send(http.MethodGet, path, "", nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", path, w.Code)
			continue
		}
		if got := w.Header().Get("Content-Type"); got != want.contentType {
			t.Errorf("%s: Content-Type = %q", path, got)
		}
		if !strings.Contains(w.Body.String(), want.body) {
			t.Errorf("%s does not contain %q", path, want.body)
		}
	}
	page := p.send(http.MethodGet, "/docs", "", nil)
	if !strings.Contains(page.Body.String(), "<table>") {
		t.Error("tables were not rendered")
	}
	if got := page.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q", got)
	}
}

// Docs for routes that do not exist would only mislead.
func TestTheDocsGoWhenThePublicAPIIsOff(t *testing.T) {
	p := newPublicAPI(t)
	p.PublicAPIEnabled = false
	if w := p.send(http.MethodGet, "/docs", "", nil); w.Code == http.StatusOK {
		t.Error("the docs are served with the public API off")
	}
}
