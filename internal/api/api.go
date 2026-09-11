// Package api serves the HTTP surface: order creation and status, the SEP-1
// stellar.toml, SEP-10 authentication, and the trustline preflight.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/money"
	"github.com/Rinku-Labs/linq-stellar/internal/payout"
	"github.com/Rinku-Labs/linq-stellar/internal/sep"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Server holds the dependencies the handlers need.
type Server struct {
	DB    *gorm.DB
	Chain *stellar.Client
	Auth  *sep.Authenticator // nil when SEP-10 is not configured
	TOML  sep.TOMLConfig

	// DepositLookup asks the Linq backend whether an address is one of its
	// deposit accounts. Consumer-app orders live there rather than here, so
	// without it fee sponsorship would refuse every one of them. Optional: with
	// no lookup configured only this service's own orders are sponsored.
	DepositLookup DepositLookup

	HomeDomain    string
	DepositWindow time.Duration
	// URISigner signs SEP-7 payment requests. Optional: unsigned URIs still
	// work, wallets just show them as unverified.
	URISigner *sep.URISigner
	// OrdersAPIKey guards order creation and lookup. Every other route stays
	// open: the toml and trustline preflight are meant for anyone to call, and
	// SEP-10 is its own authentication. Required by cmd/server before it will
	// listen — never zero-value in a running server.
	OrdersAPIKey string
	Log          *slog.Logger
}

// requireAPIKey wraps a handler with the shared-secret check for order routes.
//
// Without this, POST /orders is a free way to make the sponsor pay reserves
// for an account nobody ever funds — cheap to spam, and each one sits there
// until its deposit window expires. The comparison is constant-time so a
// caller cannot learn the key one byte at a time from response latency.
func (s *Server) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-API-Key")
		if provided == "" ||
			subtle.ConstantTimeCompare([]byte(provided), []byte(s.OrdersAPIKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid or missing X-API-Key")
			return
		}
		next(w, r)
	}
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
	// Unauthenticated by design; the transaction is the credential. See
	// handleSubmit.
	mux.HandleFunc("POST /stellar/submit", s.handleSubmit)
	mux.HandleFunc("POST /orders", s.requireAPIKey(s.handleCreateOrder))
	mux.HandleFunc("GET /orders/{id}", s.requireAPIKey(s.handleOrderStatus))

	// Preflight for the routes browsers call cross-origin. Registered
	// explicitly because ServeMux matches on method, so an OPTIONS request
	// would otherwise fall through to 405 and the real request never happens.
	mux.HandleFunc("OPTIONS /", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	return withCORS(logRequests(s.Log, mux))
}

// withCORS lets browser clients call this service cross-origin.
//
// SEP-10 exists to be called from a wallet or a web app on another domain —
// that is the whole point of publishing WEB_AUTH_ENDPOINT in a stellar.toml for
// others to discover. Without these headers the browser blocks the response
// before any application code sees it, and the caller gets an opaque "failed to
// fetch" that says nothing about why.
//
// The origin is open because the endpoints behind it are either public
// (stellar.toml, SEP-10 challenges, trustline lookups) or independently
// authenticated by an API key that a browser on another origin does not hold.
// Allowing the origin does not grant access; it only allows the reply to be
// read.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleTOML(w http.ResponseWriter, r *http.Request) {
	// CORS is applied for every route by withCORS.
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

// DepositLookup reports whether an address is a deposit account the Linq
// backend minted and is still expecting payment into.
type DepositLookup interface {
	LookupDepositAddress(address string) (payout.KnownDepositAddress, error)
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

	// The quote, struck before anything is provisioned. Whichever side the
	// caller supplied, the other is derived here and both are then fixed for
	// the life of the order.
	//
	// The USDC side rounds UP (money.QuoteUSDC) rather than to nearest. That
	// direction is the difference between asking a payer for 0.073303 USDC
	// against a ₦100 invoice and asking for 0.07 — the second is 4.5% short,
	// and it was the merchant who absorbed the difference.
	//
	// Quoting first also means a quote that cannot be struck costs nothing: an
	// unquotable order used to reach this point only after the sponsor had
	// already paid the reserves for an account nobody would ever fund.
	amountUSDC, amountNGN := req.AmountUSDC, req.AmountNGN
	if amountUSDC == 0 {
		amountUSDC, err = money.QuoteUSDC(amountNGN, req.Rate)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		// A caller who names the USDC still gets it snapped to a precision the
		// network and the payer's wallet can both express exactly.
		amountUSDC = money.CeilUSDC(amountUSDC)
	}
	if amountNGN == 0 {
		amountNGN = money.QuoteNGN(amountUSDC, req.Rate)
	} else {
		amountNGN = money.RoundNGN(amountNGN)
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

	order := store.Order{
		ID:              uuid.NewString(),
		BusinessID:      req.BusinessID,
		IdempotencyKey:  req.IdempotencyKey,
		CustomerRef:     req.CustomerRef,
		Status:          store.StateAwaitingDeposit,
		AmountUSDC:      amountUSDC,
		AmountNGN:       amountNGN,
		QuotedUSDC:      amountUSDC,
		QuotedNGN:       amountNGN,
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
		"id":             o.ID,
		"status":         o.Status,
		"depositAddress": o.DepositAddress,
		"amountUsdc":     o.AmountUSDC,
		"amountNgn":      o.AmountNGN,
		// The quote is published alongside the running amounts, not folded into
		// them. Before a deposit the two agree; after one, amountUsdc is what
		// arrived and quotedUsdc is what was asked for, and a caller that wants
		// to show a payer "you sent 0.07 of the 0.073303 due" needs both.
		"quotedUsdc":      o.QuotedUSDC,
		"quotedNgn":       o.QuotedNGN,
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
	// Only present when it happened, so a caller cannot mistake the ordinary
	// false for "we checked and it was fine" on an order predating the check.
	if o.Underpaid {
		payload["underpaid"] = true
		payload["shortfallNgn"] = o.ShortfallNGN
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

// handleSubmit fee-bumps and submits a payment the payer has already signed.
//
// This is what makes "zero fees deducted from the user's wallet" literally
// true. Without it the payer signs their own transaction and pays its network
// fee; with it the sponsor is billed for the fee and the payer's account is
// touched for nothing but the USDC they meant to send.
//
// It is deliberately unauthenticated, because the caller is a browser and any
// key shipped there is public. The credential is the transaction itself: this
// will only pay a fee for a plain USDC payment into a deposit account this
// service minted and is currently expecting money for. Anything else is
// refused, so the worst an arbitrary caller can do is pay us.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Transaction string `json:"transaction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Transaction == "" {
		writeError(w, http.StatusBadRequest, "a signed transaction is required")
		return
	}

	intent, err := stellar.InspectUSDCPayment(body.Transaction, s.Chain.USDCAsset())
	if err != nil {
		s.Log.Info("refused to sponsor a transaction", "error", err)
		writeError(w, http.StatusBadRequest,
			"this service only sponsors USDC payments into its own deposit addresses")
		return
	}

	// At least one operation must pay a deposit account Linq owns and is still
	// expecting money for. Checking the state as well as the address matters: a
	// settled order's account has been merged away, so sponsoring a payment to
	// it would burn a fee on a transaction destined to fail.
	//
	// Only one, rather than all: the consumer app can attach a savings transfer
	// to the same transaction, paying the user's own savings address alongside
	// the deposit. Demanding every destination be ours would refuse exactly the
	// payments this exists to sponsor. The fee is a fixed few stroops per
	// operation either way, and the payment reaching us is what earns it.
	sponsored := false
	for _, destination := range intent.Destinations {
		ok, err := s.isAwaitingDeposit(destination)
		if err != nil {
			s.Log.Error("could not check deposit address", "destination", destination, "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not verify the payment right now")
			return
		}
		if ok {
			sponsored = true
			break
		}
	}
	if !sponsored {
		s.Log.Info("refused to sponsor a payment to unknown addresses", "destinations", intent.Destinations)
		writeError(w, http.StatusBadRequest,
			"this service only sponsors payments into Linq deposit addresses")
		return
	}

	hash, err := s.Chain.FeeBumpAndSubmit(body.Transaction)
	if err != nil {
		s.Log.Error("could not submit fee-bumped payment", "error", err)
		writeError(w, http.StatusBadGateway, "the payment could not be submitted; nothing was sent")
		return
	}

	s.Log.Info("sponsored a payer's network fee", "tx", hash, "usdc", intent.Total)
	writeJSON(w, http.StatusOK, map[string]any{"hash": hash, "feeSponsored": true})
}


// isAwaitingDeposit reports whether an address is a Linq deposit account that
// is still expecting payment.
//
// This service's own orders are checked first because that is a local query.
// Orders created through the Linq backend — everything from the consumer app —
// are invisible here, so the backend is asked about anything unrecognised.
//
// A lookup failure is returned as an error rather than as "not ours". Treating
// an unreachable backend as a definite no would quietly stop sponsoring every
// consumer payment the moment it had a bad minute, and nobody would notice
// except payers, who would start paying their own fees again.
func (s *Server) isAwaitingDeposit(address string) (bool, error) {
	var count int64
	if err := s.DB.Model(&store.Order{}).
		Where("deposit_address = ? AND status = ?", address, store.StateAwaitingDeposit).
		Count(&count).Error; err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}

	if s.DepositLookup == nil {
		return false, nil
	}
	known, err := s.DepositLookup.LookupDepositAddress(address)
	if err != nil {
		return false, err
	}
	return known.Known && known.AwaitingDeposit, nil
}
