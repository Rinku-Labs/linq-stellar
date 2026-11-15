package api

import (
	"strings"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/stellar/go-stellar-sdk/network"
)

var proofStart = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

func proofOrder(id, integrator string, business uint, created time.Time, disbursed bool) store.Order {
	o := store.Order{
		ID: id, IntegratorID: integrator, IdempotencyKey: id, BusinessID: business, CreatedAt: created,
		Status: store.StateAwaitingDeposit, DepositTxHash: "dep" + id, AmountUSDC: 1.47, AmountNGN: 2000,
		BankAccount: "0123456789", AccountName: "Ada Obi Private", BankName: "Private Bank", BankCode: "058",
		DepositDeadline: created.Add(30 * time.Minute),
	}
	if disbursed {
		dep, paid := created.Add(time.Minute), created.Add(time.Minute+3*time.Second)
		o.Status, o.FinancialOutcome = store.StateSettledInTreasury, "disbursed"
		o.DepositedAt, o.PaidOutAt = &dep, &paid
		o.PayoutReference, o.PayoutProvider, o.SweepTxHash = "linq-payout-"+id, "nomba", "sweep"+id
	}
	return o
}

func TestTheProofPageNeedsTheReviewerToken(t *testing.T) {
	p := newReviewerPage(t, network.PublicNetworkPassphrase)
	if w := p.request("GET", "/reconciliation/proof", "", nil); w.Code == 200 {
		t.Errorf("the proof page opened without the token: %d", w.Code)
	}
}

func TestTheProofPageSaysWhenItIsNotSetUp(t *testing.T) {
	p := newReviewerPage(t, network.PublicNetworkPassphrase)
	body := p.get("/reconciliation/proof")
	if !strings.Contains(body, "isn&#39;t set up") && !strings.Contains(body, "isn't set up") {
		t.Errorf("an unset proof page doesn't say so:\n%s", body)
	}
}

func TestTheProofPageCountsOnlyTheScopedOrders(t *testing.T) {
	p := newReviewerPage(t, network.PublicNetworkPassphrase)
	p.s.Proof = store.ProofScope{IntegratorID: "int_v2", BusinessID: 7, Since: proofStart}
	p.s.ProofDemoIntegrator = "int_demo"

	orders := []store.Order{
		proofOrder("before", "int_v2", 7, proofStart.Add(-time.Hour), true),    // before the switch
		proofOrder("otherbiz", "int_v2", 8, proofStart.Add(time.Hour), true),   // another business
		proofOrder("otherint", "int_x", 7, proofStart.Add(time.Hour), true),    // another integrator
		proofOrder("paid1", "int_v2", 7, proofStart.Add(time.Hour), true),      // counted
		proofOrder("paid2", "int_v2", 7, proofStart.Add(2*time.Hour), true),    // counted
		proofOrder("waiting", "int_v2", 7, proofStart.Add(3*time.Hour), false), // a payment, not yet paid out
	}
	transient := proofOrder("demoT", "int_demo", 0, proofStart, true)
	transient.InducedFailure = store.InducedTransient
	terminal := proofOrder("demoX", "int_demo", 0, proofStart, false)
	terminal.InducedFailure, terminal.Status, terminal.FinancialOutcome, terminal.RefundTxHash =
		store.InducedTerminal, store.StateRefunded, "refunded", "refundX"
	orders = append(orders, transient, terminal)
	for i := range orders {
		if err := p.db.Create(&orders[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	p.db.Create(&store.StatusEvent{OrderID: "paid1", Kind: store.KindTransition, To: store.StateDisbursed,
		At: proofStart.Add(time.Hour), NotifiedAt: at(proofStart.Add(time.Hour + 2*time.Second))})
	for i := 0; i < 11; i++ {
		finished := proofStart.Add(time.Duration(i*15+1) * time.Minute)
		p.cycle(proofStart.Add(time.Duration(i*15)*time.Minute), &finished, "")
	}

	body := p.get("/reconciliation/proof")
	for _, want := range []string{
		"paid1", "paid2", "waiting", "linq-payout-paid1",
		"stellar.expert/explorer/public/tx/deppaid1",
		"Retried and paid", "Refunded on-chain", "stellar.expert/explorer/public/tx/refundX",
		"induced on purpose", "earlier implementation",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the proof page is missing %q", want)
		}
	}
	for _, unwanted := range []string{"before", "otherbiz", "otherint"} {
		if strings.Contains(body, "<code>"+unwanted+"</code>") {
			t.Errorf("the proof page counts %q, which is out of scope", unwanted)
		}
	}
	for _, private := range []string{"0123456789", "Ada Obi Private", "Private Bank"} {
		if strings.Contains(body, private) {
			t.Errorf("the proof page shows %q", private)
		}
	}
	// 3 payments, 2 disbursements (under the target of 5), 11 cycles (over 10).
	if !strings.Contains(body, "<b>3</b>") || !strings.Contains(body, `<b class="">2</b>`) || !strings.Contains(body, `<b class="ok">11</b>`) {
		t.Errorf("the counts are wrong:\n%s", body)
	}
}
