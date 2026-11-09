package config

import (
	"strings"
	"testing"
	"time"
)

func required(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"DATABASE_URL":            "postgres://localhost/test",
		"STELLAR_SPONSOR_KEY":     "SXXX",
		"STELLAR_TREASURY_WALLET": "GXXX",
		"ENCRYPTION_KEY":          "0123456789abcdef0123456789abcdef",
		"LINQ_API_URL":            "https://linq.example",
		"LINQ_INTERNAL_SECRET":    "secret",
	} {
		t.Setenv(k, v)
	}
}

// The reviewer token is the reconciliation page's only lock, and it is handed
// to people outside the team.
func TestAShortReviewerTokenIsRefused(t *testing.T) {
	required(t)
	t.Setenv("REVIEWER_TOKEN", "short")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REVIEWER_TOKEN must be at least 32 characters") {
		t.Errorf("short token: %v", err)
	}
	t.Setenv("REVIEWER_TOKEN", strings.Repeat("a", 64))
	if c, err := Load(); err != nil || c.ReviewerToken == "" {
		t.Errorf("long token: %v", err)
	}
	t.Setenv("REVIEWER_TOKEN", "")
	if c, err := Load(); err != nil || c.ReviewerToken != "" {
		t.Errorf("no token is allowed, and means no page: %v", err)
	}
}

func TestReconciliationSettings(t *testing.T) {
	required(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ReconcileInterval.Minutes() != 15 || c.ReconcileHorizonCalls != 200 || c.ReconcilePayoutCalls != 50 || c.ReconcileSponsoredAccounts {
		t.Errorf("defaults: %+v", c)
	}
	t.Setenv("RECONCILE_INTERVAL", "10s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "RECONCILE_INTERVAL must be at least 1m") {
		t.Errorf("an interval under a minute: %v", err)
	}
}

func TestProofSettings(t *testing.T) {
	required(t)
	t.Setenv("PROOF_INTEGRATOR_ID", "int_v2")
	t.Setenv("PROOF_SINCE", "yesterday")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PROOF_SINCE") {
		t.Errorf("an unreadable PROOF_SINCE was accepted: %v", err)
	}
	t.Setenv("PROOF_SINCE", "")
	if _, err := Load(); err == nil {
		t.Errorf("PROOF_INTEGRATOR_ID without PROOF_SINCE was accepted")
	}
	t.Setenv("PROOF_SINCE", "2026-11-09T10:00:00Z")
	t.Setenv("PROOF_BUSINESS_ID", "7")
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ProofIntegratorID != "int_v2" || c.ProofBusinessID != 7 || c.ProofSince.Format(time.RFC3339) != "2026-11-09T10:00:00Z" {
		t.Errorf("proof settings = %q %d %v", c.ProofIntegratorID, c.ProofBusinessID, c.ProofSince)
	}
}
