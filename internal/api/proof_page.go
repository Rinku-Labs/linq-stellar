package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

// The D4 proof page shows a reviewer, live, the records behind the SCF
// Deliverable 4 success criteria: the integrator's Stellar payments since
// routing was switched on, the NGN disbursements, the reconciliation cycles,
// the induced failure demonstrations and how fast merchants were told.
//
// Unlike the rest of the reviewer pages it reads the orders table, through
// store.ProofColumns only: no bank account, account name or bank name.

const (
	proofDisbursementsTarget = 5
	proofCyclesTarget        = 10
	proofNotificationLimit   = 60 * time.Second
)

type proofOrderRow struct {
	OrderID, Created, Status, USDC, NGN      string
	PayoutRef, Provider, Seconds             string
	DepositLink, SweepLink, RefundLink, Kind string
	Outcome                                  string
	Good                                     bool
}

type proofView struct {
	Refresh    int
	Configured bool
	Network    string
	Since      string
	BusinessID uint

	Payments      []proofOrderRow
	Disbursements []proofOrderRow
	Demos         []proofOrderRow

	DisbursementsTarget, CyclesTarget int
	Completed, Mismatches, Unresolved int64
	Delivered, LateNotices            int
	Slowest                           string
	DisbursementsMet, CyclesMet       bool
}

func (s *Server) handleProof(w http.ResponseWriter, r *http.Request) {
	view := proofView{
		Refresh: pageRefresh, Network: s.explorerNetwork(),
		DisbursementsTarget: proofDisbursementsTarget, CyclesTarget: proofCyclesTarget,
		Configured: s.Proof.IntegratorID != "",
	}
	var err error
	if view.Completed, err = store.CompletedCycles(s.DB); err != nil {
		s.pageFailed(w, "completed cycles", err)
		return
	}
	if view.Mismatches, view.Unresolved, err = store.MismatchCounts(s.DB); err != nil {
		s.pageFailed(w, "mismatch counts", err)
		return
	}

	if view.Configured {
		view.Since, view.BusinessID = when(s.Proof.Since), s.Proof.BusinessID
		orders, err := store.ProofOrders(s.DB, s.Proof)
		if err != nil {
			s.pageFailed(w, "proof orders", err)
			return
		}
		ids := make([]string, 0, len(orders))
		for _, o := range orders {
			ids = append(ids, o.ID)
			row := s.proofRow(o)
			view.Payments = append(view.Payments, row)
			if o.FinancialOutcome == "disbursed" {
				view.Disbursements = append(view.Disbursements, row)
			}
		}
		var slowest time.Duration
		view.Delivered, view.LateNotices, slowest, err = store.NotificationTimes(s.DB, ids, proofNotificationLimit)
		if err != nil {
			s.pageFailed(w, "notification times", err)
			return
		}
		view.Slowest = fmt.Sprintf("%.1f s", slowest.Seconds())
	}

	if s.ProofDemoIntegrator != "" {
		demos, err := store.InducedOrders(s.DB, s.ProofDemoIntegrator)
		if err != nil {
			s.pageFailed(w, "demo orders", err)
			return
		}
		for _, o := range demos {
			row := s.proofRow(o)
			row.Kind = o.InducedFailure
			switch {
			case o.InducedFailure == store.InducedTransient && o.FinancialOutcome == "disbursed":
				row.Outcome, row.Good = "Retried and paid", true
			case o.InducedFailure == store.InducedTerminal && o.Status == store.StateRefunded && o.RefundTxHash != "":
				row.Outcome, row.Good = "Refunded on-chain", true
			default:
				row.Outcome = "In progress: " + o.Status
			}
			view.Demos = append(view.Demos, row)
		}
	}
	view.DisbursementsMet = len(view.Disbursements) >= proofDisbursementsTarget
	view.CyclesMet = view.Completed >= proofCyclesTarget
	s.renderPage(w, http.StatusOK, proofPage, view)
}

func (s *Server) proofRow(o store.Order) proofOrderRow {
	net := s.explorerNetwork()
	row := proofOrderRow{
		OrderID: o.ID, Created: when(o.CreatedAt), Status: o.Status,
		USDC: fmt.Sprintf("%g", o.AmountUSDC), NGN: fmt.Sprintf("%.2f", o.AmountNGN),
		PayoutRef: o.PayoutReference, Provider: o.PayoutProvider,
		DepositLink: explorerLink(net, "tx", o.DepositTxHash),
		SweepLink:   explorerLink(net, "tx", o.SweepTxHash),
		RefundLink:  explorerLink(net, "tx", o.RefundTxHash),
	}
	if o.DepositedAt != nil && o.PaidOutAt != nil {
		row.Seconds = fmt.Sprintf("%.0f s", o.PaidOutAt.Sub(*o.DepositedAt).Seconds())
	}
	return row
}

const proofHTML = `{{define "refresh"}}<meta http-equiv="refresh" content="{{.Refresh}}">{{end}}
{{define "content"}}{{template "header" .}}<main>
  <h1>Deliverable 4 proof</h1>
  {{if .Configured}}
  <p class="lead">Live records behind the SCF Deliverable 4 criteria, on the Stellar {{.Network}} network. Only orders created from <b>{{.Since}}</b>{{if .BusinessID}} by business {{.BusinessID}}{{end}} count: before that moment, this business's Stellar orders settled through an earlier implementation. The business's requests enter through Linq's existing B2B endpoint, which calls this public settlement API. This page reloads every minute.</p>
  {{else}}
  <p class="lead">The live proof isn't set up on this deployment yet (PROOF_INTEGRATOR_ID and PROOF_SINCE). Reconciliation and the failure demonstrations are shown below.</p>
  {{end}}
  <div class="cards">
    {{if .Configured}}
    <div class="card"><span>Stellar payments through the API</span><b>{{len .Payments}}</b></div>
    <div class="card"><span>NGN disbursements (target {{.DisbursementsTarget}})</span><b class="{{if .DisbursementsMet}}ok{{end}}">{{len .Disbursements}}</b></div>
    <div class="card"><span>Merchant notices, over 60 s</span><b class="{{if .LateNotices}}mismatch{{else}}ok{{end}}">{{.LateNotices}} of {{.Delivered}}</b><span>slowest {{.Slowest}}</span></div>
    {{end}}
    <div class="card"><span>Reconciliation cycles (target {{.CyclesTarget}})</span><b class="{{if .CyclesMet}}ok{{end}}">{{.Completed}}</b><span><a href="/reconciliation">See every cycle</a></span></div>
    <div class="card"><span>Mismatches, not yet resolved</span><b>{{.Unresolved}} of {{.Mismatches}}</b><span><a href="/reconciliation/mismatches">See them</a></span></div>
  </div>

  {{if .Configured}}
  <h2>NGN disbursements</h2>
  <p class="lead">The payout reference is the merchant reference Linq's payout partner, Nomba, holds for the bank transfer.</p>
  <div class="scroll"><table>
    <thead><tr><th>Order</th><th>Created</th><th class="num">NGN</th><th>Payout reference</th><th>Provider</th><th>Deposit to payout</th><th>On Stellar Expert</th></tr></thead>
    <tbody>
    {{range .Disbursements}}<tr>
      <td><code>{{.OrderID}}</code></td><td>{{.Created}}</td><td class="num">{{.NGN}}</td>
      <td><code>{{.PayoutRef}}</code></td><td>{{.Provider}}</td><td>{{.Seconds}}</td>
      <td>{{if .DepositLink}}<a href="{{.DepositLink}}" rel="noopener noreferrer" target="_blank">Deposit</a>{{end}}{{if .SweepLink}}<br><a href="{{.SweepLink}}" rel="noopener noreferrer" target="_blank">Sweep</a>{{end}}</td>
    </tr>{{else}}<tr><td colspan="7" class="muted">None yet.</td></tr>{{end}}
    </tbody>
  </table></div>

  <h2>Every Stellar payment</h2>
  <div class="scroll"><table>
    <thead><tr><th>Order</th><th>Created</th><th>Status</th><th class="num">USDC</th><th>On Stellar Expert</th></tr></thead>
    <tbody>
    {{range .Payments}}<tr>
      <td><code>{{.OrderID}}</code></td><td>{{.Created}}</td><td>{{.Status}}</td><td class="num">{{.USDC}}</td>
      <td>{{if .DepositLink}}<a href="{{.DepositLink}}" rel="noopener noreferrer" target="_blank">Deposit</a>{{end}}{{if .RefundLink}}<br><a href="{{.RefundLink}}" rel="noopener noreferrer" target="_blank">Refund</a>{{end}}</td>
    </tr>{{else}}<tr><td colspan="5" class="muted">None yet.</td></tr>{{end}}
    </tbody>
  </table></div>
  {{end}}

  <h2>Failure demonstrations</h2>
  <p class="lead">These payout failures were induced on purpose, on Linq's own test orders. An induced attempt never reaches the payout provider. The retry, the refund and the merchant notice that follow ran as they do for any order. Each order's full timeline is on <code>GET /v1/orders/{id}</code>.</p>
  <div class="scroll"><table>
    <thead><tr><th>Order</th><th>Failure</th><th>Outcome</th><th class="num">USDC</th><th>On Stellar Expert</th></tr></thead>
    <tbody>
    {{range .Demos}}<tr>
      <td><code>{{.OrderID}}</code><br><span class="muted">{{.Created}}</span></td><td>{{.Kind}}</td>
      <td class="{{if .Good}}ok{{end}}">{{.Outcome}}</td><td class="num">{{.USDC}}</td>
      <td>{{if .DepositLink}}<a href="{{.DepositLink}}" rel="noopener noreferrer" target="_blank">Deposit</a>{{end}}{{if .RefundLink}}<br><a href="{{.RefundLink}}" rel="noopener noreferrer" target="_blank">Refund</a>{{end}}{{if .SweepLink}}<br><a href="{{.SweepLink}}" rel="noopener noreferrer" target="_blank">Sweep</a>{{end}}</td>
    </tr>{{else}}<tr><td colspan="5" class="muted">None recorded.</td></tr>{{end}}
    </tbody>
  </table></div>
</main>{{end}}`

var proofPage = page(proofHTML)
