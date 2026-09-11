package payout

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// KnownDepositAddress reports what the Linq backend says about a Stellar
// address: whether it minted it, and whether it is still expecting a payment.
type KnownDepositAddress struct {
	Known           bool `json:"known"`
	AwaitingDeposit bool `json:"awaitingDeposit"`
}

// LookupDepositAddress asks the Linq backend whether an address is one of its
// deposit accounts.
//
// Consumer-app orders are created in that backend, not here, so their deposit
// addresses are invisible to this service. Fee sponsorship checks the
// destination before paying, and without this every consumer payment would be
// refused and the payer would go on paying their own network fee.
//
// A failure is reported as an error rather than as "not ours". Treating an
// unreachable backend as a definite no would silently switch fee sponsorship
// off for everyone the moment it had a bad minute.
func (l *LinqAPI) LookupDepositAddress(address string) (KnownDepositAddress, error) {
	endpoint := fmt.Sprintf("%s/internal/deposit-address?address=%s",
		l.BaseURL, url.QueryEscape(address))

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return KnownDepositAddress{}, fmt.Errorf("payout: build deposit lookup: %w", err)
	}
	req.Header.Set("X-Internal-Secret", l.Secret)

	res, err := l.HTTP.Do(req)
	if err != nil {
		return KnownDepositAddress{}, fmt.Errorf("payout: deposit lookup: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode >= 300 {
		return KnownDepositAddress{}, fmt.Errorf("payout: deposit lookup returned %d", res.StatusCode)
	}

	var body KnownDepositAddress
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return KnownDepositAddress{}, fmt.Errorf("payout: decode deposit lookup: %w", err)
	}
	return body, nil
}
