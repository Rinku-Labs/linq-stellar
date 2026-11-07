package notify

import (
	"context"
	"reflect"
	"slices"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/Rinku-Labs/linq-stellar/docs"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"gopkg.in/yaml.v3"
)

// This function is the snippet in docs/api/README.md, character for character;
// the test below checks that.
func verifyLinqSignature(body []byte, header, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(header))
}

// What integrators are told a delivery contains must be what one contains.
func TestTheDocumentedEventMatchesTheEvent(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(docs.OpenAPI, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	documented := spec.Components.Schemas["WebhookEvent"].Properties

	var sent []string
	event := reflect.TypeOf(Event{})
	for i := range event.NumField() {
		name, _, _ := strings.Cut(event.Field(i).Tag.Get("json"), ",")
		// Linq-B2B only: integrators never receive it.
		if name != "payoutProvider" {
			sent = append(sent, name)
		}
	}
	for _, name := range sent {
		if _, ok := documented[name]; !ok {
			t.Errorf("%s is sent but not documented", name)
		}
	}
	for name := range documented {
		if !slices.Contains(sent, name) {
			t.Errorf("%s is documented but not sent", name)
		}
	}

	_, section, _ := strings.Cut(string(docs.Guide), "## Webhooks")
	section, _, _ = strings.Cut(section, "## Errors")
	for state := range notifiable {
		if !strings.Contains(section, "`"+state+"`") {
			t.Errorf("%s is delivered but not in the guide", state)
		}
	}
}

func TestTheDocumentedSignatureCheckAcceptsARealDelivery(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "top-secret")
	seedOrder(t, w.DB, "ord_doc", store.StateDepositDetected)
	move(t, w.DB, "ord_doc", store.StateDepositDetected, store.StateDisbursed)
	w.sweep(context.Background())

	body, sig := <-r.bodies, <-r.sigs
	if !verifyLinqSignature(body, sig, "top-secret") {
		t.Error("the documented check refused a real delivery")
	}
	if verifyLinqSignature(body, sig, "wrong-secret") {
		t.Error("the documented check accepted the wrong secret")
	}
	if verifyLinqSignature(append(body, ' '), sig, "top-secret") {
		t.Error("the documented check accepted a changed body")
	}

	source, err := os.ReadFile("docs_test.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	normalise := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	code := normalise(string(source))
	start := strings.Index(code, "func verifyLinqSignature")
	snippet := code[start : start+strings.Index(code[start:], "\n}\n")+3]
	if !strings.Contains(normalise(string(docs.Guide)), snippet) {
		t.Error("the snippet in docs/api/README.md differs from the tested function")
	}
}
