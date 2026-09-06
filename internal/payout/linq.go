package payout

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// LinqAPI disburses NGN through the Linq B2B API.
//
// It targets two internal endpoints, guarded by the shared-secret header the
// Linq API already uses for service-to-service calls:
//
//	POST /internal/payout          {reference, amountNGN, currency, bankCode,
//	                                bankAccount, accountName, bankName,
//	                                sourceChain, sourceTxHash}
//	  -> 200/202 {status, payoutId, provider}
//
//	GET  /internal/payout?reference=<id>
//	  -> 200 {status, payoutId, provider, failureReason}
//
// Both must be idempotent on reference. The public POST /b2b/offramp cannot
// serve this: it mints its own deposit wallet and waits for a deposit, which is
// exactly the half this service has already done.
type LinqAPI struct {
	BaseURL string
	// Secret is sent as X-Internal-Secret.
	Secret string
	HTTP   *http.Client
}

// NewLinqAPI returns a client with a bounded timeout.
//
// The timeout matters more than it looks: a payout request that hangs holds an
// order in payout_processing, and the retry that follows is the one that could
// double-pay if the endpoint were not idempotent.
func NewLinqAPI(baseURL, secret string) (*LinqAPI, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("payout: linq api base url is not set")
	}
	if secret == "" {
		return nil, fmt.Errorf("payout: linq internal secret is not set")
	}
	return &LinqAPI{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Secret:  secret,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

type payoutResponse struct {
	Status        string `json:"status"`
	PayoutID      string `json:"payoutId"`
	Provider      string `json:"provider"`
	FailureReason string `json:"failureReason"`
	Message       string `json:"message"`
}

// Pay submits a disbursement.
func (l *LinqAPI) Pay(r Request) (Result, error) {
	if err := r.Validate(); err != nil {
		return Result{}, err
	}

	body, err := json.Marshal(map[string]any{
		"reference":    r.Reference,
		"amountNGN":    r.AmountNGN,
		"currency":     defaultString(r.Currency, "NGN"),
		"bankCode":     r.BankCode,
		"bankAccount":  r.BankAccount,
		"accountName":  r.AccountName,
		"bankName":     r.BankName,
		"sourceChain":  "stellar",
		"sourceTxHash": r.SourceTxHash,
	})
	if err != nil {
		return Result{}, fmt.Errorf("payout: encode request: %w", err)
	}

	resp, err := l.do(http.MethodPost, l.BaseURL+"/internal/payout", body)
	if err != nil {
		return Result{}, err
	}
	return toResult(resp), nil
}

// Status reports on a previously submitted payout.
func (l *LinqAPI) Status(reference string) (Result, error) {
	if reference == "" {
		return Result{}, fmt.Errorf("payout: reference is required")
	}
	resp, err := l.do(http.MethodGet,
		fmt.Sprintf("%s/internal/payout?reference=%s", l.BaseURL, reference), nil)
	if err != nil {
		return Result{}, err
	}
	return toResult(resp), nil
}

func (l *LinqAPI) do(method, url string, body []byte) (payoutResponse, error) {
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return payoutResponse{}, fmt.Errorf("payout: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", l.Secret)

	res, err := l.HTTP.Do(req)
	if err != nil {
		// A transport failure is not evidence the payout did not happen — the
		// request may have arrived and the response been lost. The caller must
		// resolve this with Status rather than by resubmitting blindly.
		return payoutResponse{}, fmt.Errorf("payout: %s %s: %w", method, url, err)
	}
	defer res.Body.Close()

	var parsed payoutResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil && res.StatusCode < 300 {
		return payoutResponse{}, fmt.Errorf("payout: decode response: %w", err)
	}

	if res.StatusCode >= 300 {
		detail := parsed.Message
		if detail == "" {
			detail = parsed.FailureReason
		}
		return payoutResponse{}, fmt.Errorf("payout: %s returned %d: %s", url, res.StatusCode, detail)
	}
	return parsed, nil
}

// toResult maps the API's status vocabulary onto ours.
//
// Anything unrecognised is treated as queued rather than as failed. Guessing
// "failed" would trigger a refund for a payout that may well be in flight, and
// refunding a merchant who is about to be paid is worse than waiting.
func toResult(r payoutResponse) Result {
	status := StatusQueued
	switch strings.ToLower(r.Status) {
	case "paid", "disbursed", "completed", "success", "successful":
		status = StatusPaid
	case "failed", "error", "reversed", "declined":
		status = StatusFailed
	}
	return Result{
		Status:        status,
		Reference:     r.PayoutID,
		Provider:      r.Provider,
		FailureReason: r.FailureReason,
	}
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
