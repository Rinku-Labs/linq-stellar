package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

const goldenPath = "testdata/legacy_delivery.golden"

var sentAt = regexp.MustCompile(`"timestamp":"[^"]+"`)

// Linq-B2B must see no change in URL, payload or signature header. The golden
// file was captured from the code before per-integrator routing existed, so this
// compares against what really shipped. Only the send time may differ.
func TestLegacyDeliveryIsByteIdentical(t *testing.T) {
	var got bytes.Buffer
	var rawBody []byte
	var signature string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		signature = r.Header.Get("x-linq-signature")

		fmt.Fprintf(&got, "%s %s\n", r.Method, r.URL.RequestURI())
		names := make([]string, 0, len(r.Header))
		for name := range r.Header {
			if name != "X-Linq-Signature" && name != "Content-Length" {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&got, "%s: %s\n", name, strings.Join(r.Header[name], ","))
		}
		fmt.Fprintf(&got, "\n%s\n", sentAt.ReplaceAll(rawBody, []byte(`"timestamp":"<sent>"`)))
	}))
	defer srv.Close()

	w := newWorker(t, srv.URL+"/api/webhooks/stellar", "top-secret")
	order := store.Order{
		ID: "ord_golden", IdempotencyKey: "idem_golden", Status: store.StateRefundQueued,
		AmountNGN: 9940, AmountUSDC: 6.1, QuotedNGN: 10000, QuotedUSDC: 6.14, Rate: 1629,
		DepositAddress: "GDEPOSIT", DepositFrom: "GPAYER", DepositTxHash: "deposit-tx",
		Underpaid: true, ShortfallNGN: 60, PayoutReference: "payout-ref", PayoutProvider: "provider-a",
		SweepTxHash: "sweep-tx", RefundTxHash: "refund-tx",
	}
	if err := w.DB.Create(&order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	event := store.StatusEvent{
		OrderID: order.ID, Kind: store.KindTransition, From: store.StatePayoutProcessing, To: store.StateRefundQueued,
		Reason: "account closed", At: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
	}
	if err := w.DB.Create(&event).Error; err != nil {
		t.Fatalf("seed event: %v", err)
	}

	w.sweep(context.Background())

	if got.Len() == 0 {
		t.Fatal("nothing was delivered")
	}
	mac := hmac.New(sha256.New, []byte("top-secret"))
	mac.Write(rawBody)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); signature != want {
		t.Errorf("signature = %q, want %q", signature, want)
	}

	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if normalised := bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")); !bytes.Equal(got.Bytes(), normalised) {
		t.Errorf("delivery changed\n got: %s\nwant: %s", got.Bytes(), normalised)
	}
}
