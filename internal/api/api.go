// Package api serves the HTTP surface: order creation and status, the SEP-1
// stellar.toml, SEP-10 authentication, and the trustline preflight.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/Rinku-Labs/linq-stellar/internal/sep"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"gorm.io/gorm"
)

// Server holds the dependencies the handlers need.
type Server struct {
	DB    *gorm.DB
	Chain *stellar.Client
	Auth  *sep.Authenticator // nil when SEP-10 is not configured
	TOML  sep.TOMLConfig

	HomeDomain    string
	DepositWindow time.Duration
	// URISigner signs SEP-7 payment requests. Optional: unsigned URIs still
	// work, wallets just show them as unverified.
	URISigner *sep.URISigner
	Log       *slog.Logger
}

// Routes returns the service's HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// SEP-1. Served at the well-known path so wallets and anchors can discover
	// the keys that verify SEP-7 requests and SEP-10 challenges.
	mux.HandleFunc("GET /.well-known/stellar.toml", s.handleTOML)

	// SEP-10.
	mux.HandleFunc("GET /sep10/auth", s.handleChallenge)
	mux.HandleFunc("POST /sep10/auth", s.handleVerify)

	mux.HandleFunc("GET /stellar/trustline", s.handleTrustline)
	mux.HandleFunc("POST /orders", s.handleCreateOrder)
	mux.HandleFunc("GET /orders/{id}", s.handleOrderStatus)

	return logRequests(s.Log, mux)
}

func (s *Server) handleTOML(w http.ResponseWriter, r *http.Request) {
	// Wallets fetch this cross-origin, so it has to be readable from anywhere.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, s.TOML.RenderTOML())
}

func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeError(w, http.StatusNotImplemented, "SEP-10 is not configured on this deployment")
		return
	}
	account := r.URL.Query().Get("account")
	challenge, err := s.Auth.Challenge(account, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"transaction":        challenge,
		"network_passphrase": s.TOML.NetworkPassphrase,
	})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeError(w, http.StatusNotImplemented, "SEP-10 is not configured on this deployment")
		return
	}
	var body struct {
		Transaction string `json:"transaction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	token, account, err := s.Auth.Verify(body.Transaction)
	if err != nil {
		// Deliberately vague to the caller: which check failed is useful to an
		// attacker probing the handshake and useless to a legitimate wallet.
		s.Log.Info("sep10 verification failed", "error", err)
		writeError(w, http.StatusUnauthorized, "challenge verification failed")
		return
	}
	s.Log.Info("sep10 authenticated", "account", account)
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// handleTrustline reports whether an address can receive USDC.
//
// The distinction between the two failure modes is the point. "This address
// cannot receive USDC" is an answer; "Horizon is unreachable" is not, and
// returning the second as the first would turn a merchant away over a network
// blip.
func (s *Server) handleTrustline(w http.ResponseWriter, r *http.Request) {
	address := r.URL.Query().Get("address")
	if address == "" {
		writeError(w, http.StatusBadRequest, "address is required")
		return
	}

	trusts, err := s.Chain.TrustsUSDC(address)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable,
			"could not check the address right now; try again shortly")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"address":     address,
		"trustsUSDC":  trusts,
		"assetCode":   "USDC",
		"assetIssuer": s.Chain.USDCAsset().Issuer,
	})
}

// CreateOrderRequest is the payload for a new Stellar off-ramp order.
type CreateOrderRequest struct {
	AmountNGN      float64 `json:"amountNgn"`
	AmountUSDC     float64 `json:"amountUsdc"`
	Rate           float64 `json:"rate"`
	BusinessID     uint    `json:"businessId"`
	BankCode       string  `json:"bankCode"`
	BankAccount    string  `json:"bankAccount"`
	AccountName    string  `json:"accountName"`
	BankName       string  `json:"bankName"`
	Currency       string  `json:"currency"`
	CustomerRef    string  `json:"customerRef"`
	RefundAddress  string  `json:"refundAddress"`
	ManualDeposit  bool    `json:"manualDeposit"`
	IdempotencyKey string  `json:"idempotencyKey"`
}

func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	var req CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateCreate(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Idempotency first: a retried create must return the original order rather
	// than provision a second deposit account and leave the first stranded.
	var existing store.Order
	err := s.DB.Where("idempotency_key = ?", req.IdempotencyKey).First(&existing).Error
	if err == nil {
		s.writeOrder(w, http.StatusOK, &existing)
		return
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		writeError(w, http.StatusInternalServerError, "could not check for an existing order")
		return
	}

	// A refund address that cannot receive USDC is worth catching now, while
	// the payer is still here, rather than after a payout has failed.
	if req.RefundAddress != "" {
		trusts, err := s.Chain.TrustsUSDC(req.RefundAddress)
		if err == nil && !trusts {
			writeError(w, http.StatusBadRequest,
				"refundAddress cannot receive USDC: it needs a USDC trustline")
			return
		}
	}

	address, encryptedSeed, err := s.Chain.GenerateAccount()
	if err != nil {
		s.Log.Error("could not generate deposit account", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create a deposit address")
		return
	}

	// Provision before the address is ever shown. An unprovisioned address
	// rejects USDC, so publishing one would take the payer's payment and bounce
	// it.
	provisionTx, err := s.Chain.Provision(encryptedSeed)
	if err != nil {
		s.Log.Error("could not provision deposit account", "address", address, "error", err)
		writeError(w, http.StatusServiceUnavailable,
			"could not prepare a deposit address right now; try again shortly")
		return
	}

	amountUSDC, amountNGN := req.AmountUSDC, req.AmountNGN
	if amountUSDC == 0 && req.Rate > 0 {
		amountUSDC = amountNGN / req.Rate
	}
	if amountNGN == 0 {
		amountNGN = amountUSDC * req.Rate
	}

	order := store.Order{
		ID:              uuid.NewString(),
		BusinessID:      req.BusinessID,
		IdempotencyKey:  req.IdempotencyKey,
		CustomerRef:     req.CustomerRef,
		Status:          store.StateAwaitingDeposit,
		AmountUSDC:      amountUSDC,
		AmountNGN:       amountNGN,
		Rate:            req.Rate,
		FeeUSDC:         0, // Stellar is the zero-fee rail; stored, not implied
		DepositAddress:  address,
		EncryptedSeed:   encryptedSeed,
		ProvisionTxHash: provisionTx,
		RefundAddress:   req.RefundAddress,
		BankCode:        req.BankCode,
		BankAccount:     req.BankAccount,
		AccountName:     req.AccountName,
		BankName:        req.BankName,
		Currency:        defaultString(req.Currency, "NGN"),
		ManualDeposit:   req.ManualDeposit,
		DepositDeadline: time.Now().UTC().Add(s.DepositWindow),
	}

	if err := s.DB.Create(&order).Error; err != nil {
		// The account exists on-chain but no order references it. Log the seed
		// reference so the reserves can be reclaimed rather than lost.
		s.Log.Error("provisioned an account but could not save the order",
			"address", address, "provisionTx", provisionTx, "error", err)
		writeError(w, http.StatusInternalServerError, "could not create the order")
		return
	}

	s.writeOrder(w, http.StatusCreated, &order)
}

func (s *Server) handleOrderStatus(w http.ResponseWriter, r *http.Request) {
	var order store.Order
	if err := s.DB.First(&order, "id = ?", r.PathValue("id")).Error; err != nil {
		writeError(w, http.StatusNotFound, "order not found")
		return
	}
	s.writeOrder(w, http.StatusOK, &order)
}

// writeOrder renders an order along with the SEP-7 URI a wallet can scan.
//
// The URI is built here rather than by the caller because this service already
// knows the destination, asset, amount and memo. A frontend that assembled it
// would be reconstructing facts it had to be told, and would be the second
// place a wrong asset issuer could creep in.
func (s *Server) writeOrder(w http.ResponseWriter, status int, o *store.Order) {
	payload := map[string]any{
		"id":              o.ID,
		"status":          o.Status,
		"depositAddress":  o.DepositAddress,
		"amountUsdc":      o.AmountUSDC,
		"amountNgn":       o.AmountNGN,
		"rate":            o.Rate,
		"feeUsdc":         o.FeeUSDC,
		"currency":        o.Currency,
		"assetCode":       "USDC",
		"assetIssuer":     s.Chain.USDCAsset().Issuer,
		"depositDeadline": o.DepositDeadline,
		"createdAt":       o.CreatedAt,
		"updatedAt":       o.UpdatedAt,
	}
	if o.DepositTxHash != "" {
		payload["depositTxHash"] = o.DepositTxHash
	}
	if o.SweepTxHash != "" {
		payload["sweepTxHash"] = o.SweepTxHash
	}

	if o.DepositAddress != "" && !store.IsTerminal(o.Status) {
		uri, err := s.paymentURI(o)
		if err != nil {
			s.Log.Warn("could not build sep-7 uri", "order", o.ID, "error", err)
		} else {
			payload["paymentUri"] = uri
		}
	}

	writeJSON(w, status, payload)
}

func (s *Server) paymentURI(o *store.Order) (string, error) {
	uri, err := sep.BuildPayURI(sep.PayParams{
		Destination:  o.DepositAddress,
		Amount:       o.AmountUSDC,
		AssetCode:    "USDC",
		AssetIssuer:  s.Chain.USDCAsset().Issuer,
		Memo:         o.CustomerRef,
		Msg:          "Linq order " + o.ID,
		OriginDomain: s.HomeDomain,
	})
	if err != nil {
		return "", err
	}
	if s.URISigner == nil {
		return uri, nil
	}
	return s.URISigner.Sign(uri)
}

func validateCreate(r CreateOrderRequest) error {
	switch {
	case r.IdempotencyKey == "":
		return errors.New("idempotencyKey is required")
	case r.AmountNGN <= 0 && r.AmountUSDC <= 0:
		return errors.New("one of amountNgn or amountUsdc is required")
	case r.Rate <= 0:
		return errors.New("rate is required")
	case r.BankCode == "":
		return errors.New("bankCode is required")
	case r.BankAccount == "":
		return errors.New("bankAccount is required")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path, "took", time.Since(start))
	})
}
