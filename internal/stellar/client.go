// Package stellar implements the on-chain half of Linq's Stellar USDC
// settlement: minting single-use deposit accounts, detecting what lands in
// them, sweeping to treasury, and returning the sponsored reserves.
//
// The design rests on sponsored reserves. A Stellar address cannot receive USDC
// until the account exists on-chain and holds a USDC trustline, and both cost
// XLM base reserves. Funding thousands of throwaway accounts outright would be
// prohibitive, so a sponsor account pays those reserves and gets every lumen
// back when the order account is merged away. The per-order cost is therefore
// transaction fees alone, which is what makes a single-use account per order
// affordable and, in turn, what lets Linq charge the payer nothing.
package stellar

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// DefaultUSDCIssuer is Circle's canonical USDC issuer on the Stellar public
// network. Testnet USDC is a different issuer entirely, and a payment to the
// wrong one cannot be recovered, so this is never guessed — it is either this
// constant or explicitly configured.
const DefaultUSDCIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// defaultBaseFee sits well above the network minimum so orders still confirm
// when Stellar is congested. A deposit that cannot be swept because the fee was
// too low costs far more than the fee ever would.
const defaultBaseFee int64 = 10000

// Config is everything the client needs to operate. It is validated once at
// construction rather than read from the environment at each call site, so a
// misconfigured deployment fails at startup instead of at the first order.
type Config struct {
	// HorizonURL defaults to the public network.
	HorizonURL string
	// NetworkPassphrase decides which network signatures are valid on. It
	// defaults to pubnet: a wrong passphrase produces signatures that are
	// silently invalid on the intended network.
	NetworkPassphrase string
	// USDCIssuer defaults to DefaultUSDCIssuer.
	USDCIssuer string
	// SponsorSeed (S...) funds the reserves for every deposit account and
	// receives them back on merge. This is the one secret here that can move
	// money.
	SponsorSeed string
	// EncryptionKey encrypts deposit-account seeds at rest. AES-256 wants 32
	// bytes; 16 and 24 are accepted for AES-128 and AES-192.
	EncryptionKey []byte
	// BaseFee in stroops. Defaults to defaultBaseFee.
	BaseFee int64
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Client talks to Horizon and signs transactions for one sponsor account.
type Client struct {
	horizon    *horizonclient.Client
	passphrase string
	usdc       txnbuild.CreditAsset
	sponsor    *keypair.Full
	encKey     []byte
	baseFee    int64
	log        *slog.Logger
}

// New validates the configuration and returns a ready client.
//
// Every failure mode here is a deployment mistake rather than a runtime
// condition, which is why they are all caught up front: an unparseable sponsor
// seed or a wrong-length encryption key would otherwise surface as a failed
// order with a payer already waiting.
func New(cfg Config) (*Client, error) {
	if cfg.SponsorSeed == "" {
		return nil, fmt.Errorf("stellar: sponsor seed is not set")
	}
	sponsor, err := keypair.ParseFull(cfg.SponsorSeed)
	if err != nil {
		return nil, fmt.Errorf("stellar: invalid sponsor seed: %w", err)
	}

	switch len(cfg.EncryptionKey) {
	case 16, 24, 32:
	default:
		return nil, fmt.Errorf("stellar: encryption key must be 16, 24 or 32 bytes, got %d", len(cfg.EncryptionKey))
	}

	horizonURL := cfg.HorizonURL
	if horizonURL == "" {
		horizonURL = horizonclient.DefaultPublicNetClient.HorizonURL
	}
	passphrase := cfg.NetworkPassphrase
	if passphrase == "" {
		passphrase = network.PublicNetworkPassphrase
	}
	issuer := cfg.USDCIssuer
	if issuer == "" {
		issuer = DefaultUSDCIssuer
	}
	baseFee := cfg.BaseFee
	if baseFee < txnbuild.MinBaseFee {
		baseFee = defaultBaseFee
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Client{
		horizon:    &horizonclient.Client{HorizonURL: strings.TrimRight(horizonURL, "/") + "/"},
		passphrase: passphrase,
		usdc:       txnbuild.CreditAsset{Code: "USDC", Issuer: issuer},
		sponsor:    sponsor,
		encKey:     cfg.EncryptionKey,
		baseFee:    baseFee,
		log:        log,
	}, nil
}

// SponsorAddress returns the account paying reserves and receiving merges.
// Its Horizon history is a public record of every account this service has
// provisioned, which makes it the natural thing to point an auditor at.
func (c *Client) SponsorAddress() string { return c.sponsor.Address() }

// USDCAsset returns the asset this client settles in.
func (c *Client) USDCAsset() txnbuild.CreditAsset { return c.usdc }

// FormatAmount renders a float as a Stellar-native 7-decimal amount string.
// More precision than that is rejected as malformed.
func FormatAmount(v float64) string {
	return strconv.FormatFloat(v, 'f', 7, 64)
}

// describeHorizonError unwraps Horizon's problem document. Its result codes are
// the only part of a failed submission that explains what actually went wrong;
// the bare error says little more than "transaction failed".
func describeHorizonError(err error) error {
	if err == nil {
		return nil
	}
	hErr := horizonclient.GetError(err)
	if hErr == nil {
		return err
	}
	codes, codesErr := hErr.ResultCodes()
	if codesErr != nil {
		return fmt.Errorf("%s: %s", hErr.Problem.Title, hErr.Problem.Detail)
	}
	return fmt.Errorf("%s (tx: %s, ops: %v)", hErr.Problem.Title, codes.TransactionCode, codes.OperationCodes)
}
