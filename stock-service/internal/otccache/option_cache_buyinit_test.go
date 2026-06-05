package otccache

import (
	"strconv"
	"testing"

	"github.com/exbanka/contract/sitx"
	"github.com/exbanka/stock-service/internal/model"
	"github.com/shopspring/decimal"
)

// TestBuildAndMirrorRemoteOffers_BuyInitiated_Skipped asserts that a peer
// offer arriving with Direction == "buy_initiated" is NOT ingested as a
// remote listing — neither folded into the in-memory cache nor mirrored
// into the persistent remote OTCOffer table.
//
// The SI-TX OTC discovery model is seller-centric (§3, §3.1, §3.2): only
// SELLERS are published cross-bank, and the receiving (seller's) bank is
// the negotiation authority. A buy_initiated listing's poster is a BUYER,
// which has no spec wire representation. We never publish our own
// buy_initiated offers, but a NON-conformant cohort peer might still emit
// one with our proprietary `direction` field set; ingesting it would
// create a remote listing a local user could "bid" on, hitting the
// role-inversion fail-closed at openRemoteNegotiation. Drop it at the
// ingest boundary so it never becomes a discoverable/biddable remote row.
func TestBuildAndMirrorRemoteOffers_BuyInitiated_Skipped(t *testing.T) {
	// Own routing 999 ≠ the peer routing (222) used below — avoids the
	// ingestion collision guard. Restore to "111" on exit so this test's
	// global mutation cannot pollute sibling tests that rely on routing 111.
	prev := model.OwnRouting()
	model.SetOwnRouting("999")
	t.Cleanup(func() { model.SetOwnRouting(strconv.FormatInt(prev, 10)) })
	m := newFakeMirror()
	r := (&OptionRefresher{ownRouting: 999}).WithMirror(m)
	offers := []sitx.PublicOptionOffer{
		// Conformant sell-side offer — must be ingested.
		{OfferID: sitx.ForeignBankId{RoutingNumber: 222, ID: "sell-1"}, SellerID: sitx.ForeignBankId{ID: "client-9"}, Ticker: "AAPL", Amount: 3, StrikePrice: decimal.RequireFromString("200"), StrikeCurrency: "USD", Premium: decimal.RequireFromString("5"), PremiumCurrency: "USD", Direction: "sell_initiated", SettlementDate: "2026-06-11T00:00:00Z", CreatedAt: "2026-06-04T18:02:16Z"},
		// Non-conformant buy-side offer — must be skipped (not cached, not mirrored).
		{OfferID: sitx.ForeignBankId{RoutingNumber: 222, ID: "buy-1"}, SellerID: sitx.ForeignBankId{ID: "client-9"}, Ticker: "MSFT", Amount: 1, StrikePrice: decimal.RequireFromString("50"), StrikeCurrency: "USD", Premium: decimal.RequireFromString("1"), PremiumCurrency: "USD", Direction: "buy_initiated", SettlementDate: "2026-06-11T00:00:00Z", CreatedAt: "2026-06-04T18:02:16Z"},
	}
	rows := r.buildAndMirrorRemoteOffers("222", 222, offers)
	if len(rows) != 1 {
		t.Fatalf("expected 1 cached row (buy_initiated skipped), got %d", len(rows))
	}
	if rows[0].OfferID != "sell-1" {
		t.Errorf("expected the sell_initiated offer to survive, got %q", rows[0].OfferID)
	}
	// The buy_initiated offer must not have been mirrored.
	if _, ok := m.byKey["buy-1"]; ok {
		t.Errorf("buy_initiated offer must NOT be mirrored to the persistent remote table")
	}
	// It also must not appear in the reconcile seen-list (else a re-poll
	// that drops it would falsely "cancel" a row that never existed).
	for _, nid := range m.reconciled[222] {
		if nid == "buy-1" {
			t.Errorf("buy_initiated offer must not be in the reconcile seen-list")
		}
	}
}
