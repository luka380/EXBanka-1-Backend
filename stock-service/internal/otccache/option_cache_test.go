package otccache

import (
	"errors"
	"testing"
	"time"

	"github.com/exbanka/contract/sitx"
	"github.com/exbanka/stock-service/internal/model"
	"github.com/shopspring/decimal"
)

type fakeMirror struct {
	nextID     uint64
	byKey      map[string]uint64
	reconciled map[int64][]string
}

func newFakeMirror() *fakeMirror {
	return &fakeMirror{byKey: map[string]uint64{}, reconciled: map[int64][]string{}}
}
func (m *fakeMirror) UpsertRemote(o *model.OTCOffer, _ time.Time) (uint64, error) {
	key := ""
	if o.NativeID != nil {
		key = *o.NativeID
	}
	if id, ok := m.byKey[key]; ok {
		return id, nil
	}
	m.nextID++
	m.byKey[key] = m.nextID
	return m.nextID, nil
}
func (m *fakeMirror) ReconcileRemoteNotSeen(peerRouting int64, seen []string) (int64, error) {
	m.reconciled[peerRouting] = seen
	return 0, nil
}

func TestBuildAndMirrorRemoteOffers_StampsIDsAndReconciles(t *testing.T) {
	m := newFakeMirror()
	r := (&OptionRefresher{}).WithMirror(m)
	offers := []sitx.PublicOptionOffer{
		{OfferID: sitx.ForeignBankId{RoutingNumber: 111, ID: "1"}, SellerID: sitx.ForeignBankId{ID: "employee-1"}, Ticker: "BAC", Amount: 7, StrikePrice: decimal.RequireFromString("100"), StrikeCurrency: "USD", Premium: decimal.RequireFromString("10"), PremiumCurrency: "USD", Direction: "sell_initiated", SettlementDate: "2026-06-11T00:00:00Z", CreatedAt: "2026-06-04T18:02:16Z"},
		{OfferID: sitx.ForeignBankId{RoutingNumber: 111, ID: "2"}, SellerID: sitx.ForeignBankId{ID: "client-9"}, Ticker: "AAPL", Amount: 3, StrikePrice: decimal.RequireFromString("200"), StrikeCurrency: "USD", Premium: decimal.RequireFromString("5"), PremiumCurrency: "USD", Direction: "sell_initiated", SettlementDate: "2026-06-11T00:00:00Z", CreatedAt: "2026-06-04T18:02:16Z"},
	}
	rows := r.buildAndMirrorRemoteOffers("111", 111, offers)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, row := range rows {
		if row.Kind != "remote" || row.LocalID == 0 {
			t.Fatalf("row not stamped: %+v", row)
		}
	}
	if got := m.reconciled[111]; len(got) != 2 {
		t.Fatalf("reconcile seen-list = %v, want 2 entries", got)
	}
}

func TestBuildAndMirrorRemoteOffers_EmptyReconcilesAll(t *testing.T) {
	m := newFakeMirror()
	r := (&OptionRefresher{}).WithMirror(m)
	rows := r.buildAndMirrorRemoteOffers("111", 111, nil)
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
	got, ok := m.reconciled[111]
	if !ok || len(got) != 0 {
		t.Fatalf("expected reconcile called with empty seen for peer 111; got %v ok=%v", got, ok)
	}
}

type errMirror struct{ reconciled map[int64][]string }

func (m *errMirror) UpsertRemote(_ *model.OTCOffer, _ time.Time) (uint64, error) {
	return 0, errors.New("db down")
}
func (m *errMirror) ReconcileRemoteNotSeen(peerRouting int64, seen []string) (int64, error) {
	if m.reconciled == nil {
		m.reconciled = map[int64][]string{}
	}
	m.reconciled[peerRouting] = seen
	return 0, nil
}

func TestBuildAndMirrorRemoteOffers_UpsertFailureLeavesRowUnstamped(t *testing.T) {
	m := &errMirror{}
	r := (&OptionRefresher{}).WithMirror(m)
	offers := []sitx.PublicOptionOffer{
		{OfferID: sitx.ForeignBankId{RoutingNumber: 111, ID: "1"}, SellerID: sitx.ForeignBankId{ID: "employee-1"}, Ticker: "BAC", Amount: 7, StrikePrice: decimal.RequireFromString("100"), StrikeCurrency: "USD", Premium: decimal.RequireFromString("10"), PremiumCurrency: "USD", Direction: "sell_initiated", SettlementDate: "2026-06-11T00:00:00Z", CreatedAt: "2026-06-04T18:02:16Z"},
	}
	rows := r.buildAndMirrorRemoteOffers("111", 111, offers)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].LocalID != 0 {
		t.Fatalf("failed upsert should leave LocalID=0, got %d", rows[0].LocalID)
	}
	if got := m.reconciled[111]; len(got) != 0 {
		t.Fatalf("failed-upsert offer must not be in seen list; got %v", got)
	}
}

// ---------------------------------------------------------------------------
// SP-2a Task 7 Part B — ingestion collision guards
// ---------------------------------------------------------------------------

// TestBuildAndMirrorRemoteOffers_OwnRoutingPeer_Skipped verifies that when a
// peer's peerRouting equals OwnRouting(), the entire payload is rejected and
// nil is returned. Persisting such a payload would stamp routing_number=OwnRouting()
// on the mirror rows, making them look LOCAL.
func TestBuildAndMirrorRemoteOffers_OwnRoutingPeer_Skipped(t *testing.T) {
	model.SetOwnRouting("111")
	m := newFakeMirror()
	r := (&OptionRefresher{ownRouting: 111}).WithMirror(m)
	offers := []sitx.PublicOptionOffer{
		{OfferID: sitx.ForeignBankId{RoutingNumber: 111, ID: "bad-1"}, SellerID: sitx.ForeignBankId{ID: "client-9"}, Ticker: "AAPL", Amount: 3, StrikePrice: decimal.RequireFromString("200"), StrikeCurrency: "USD", Premium: decimal.RequireFromString("5"), PremiumCurrency: "USD", Direction: "sell_initiated", SettlementDate: "2026-06-11T00:00:00Z", CreatedAt: "2026-06-04T18:02:16Z"},
		{OfferID: sitx.ForeignBankId{RoutingNumber: 111, ID: "bad-2"}, SellerID: sitx.ForeignBankId{ID: "client-3"}, Ticker: "MSFT", Amount: 1, StrikePrice: decimal.RequireFromString("50"), StrikeCurrency: "USD", Premium: decimal.RequireFromString("1"), PremiumCurrency: "USD", Direction: "sell_initiated", SettlementDate: "2026-06-11T00:00:00Z", CreatedAt: "2026-06-04T18:02:16Z"},
	}
	// peerRouting == OwnRouting() → entire peer payload must be skipped.
	rows := r.buildAndMirrorRemoteOffers("111", 111, offers)
	if rows != nil {
		t.Errorf("expected nil (whole peer skipped), got %d rows", len(rows))
	}
	// No mirror calls should have been made.
	if len(m.byKey) != 0 {
		t.Errorf("expected no upsert calls, got %d entries in mirror", len(m.byKey))
	}
}

// TestBuildAndMirrorRemoteOffers_OwnRoutingOffer_Skipped verifies that an
// individual offer claiming our own routing in its OfferID is skipped even
// when the overall peerRouting differs (defense-in-depth per-offer guard).
func TestBuildAndMirrorRemoteOffers_OwnRoutingOffer_Skipped(t *testing.T) {
	model.SetOwnRouting("111")
	m := newFakeMirror()
	// peerRouting = 222 (legitimate peer); one offer claims OfferID.RoutingNumber=111.
	r := (&OptionRefresher{ownRouting: 111}).WithMirror(m)
	offers := []sitx.PublicOptionOffer{
		// Legitimate offer (different routing in OfferID — or zero, which is fine).
		{OfferID: sitx.ForeignBankId{RoutingNumber: 222, ID: "good-1"}, SellerID: sitx.ForeignBankId{ID: "client-9"}, Ticker: "AAPL", Amount: 3, StrikePrice: decimal.RequireFromString("200"), StrikeCurrency: "USD", Premium: decimal.RequireFromString("5"), PremiumCurrency: "USD", Direction: "sell_initiated", SettlementDate: "2026-06-11T00:00:00Z", CreatedAt: "2026-06-04T18:02:16Z"},
		// Collision offer: OfferID.RoutingNumber == OwnRouting() — must be skipped.
		{OfferID: sitx.ForeignBankId{RoutingNumber: 111, ID: "bad-1"}, SellerID: sitx.ForeignBankId{ID: "client-3"}, Ticker: "MSFT", Amount: 1, StrikePrice: decimal.RequireFromString("50"), StrikeCurrency: "USD", Premium: decimal.RequireFromString("1"), PremiumCurrency: "USD", Direction: "sell_initiated", SettlementDate: "2026-06-11T00:00:00Z", CreatedAt: "2026-06-04T18:02:16Z"},
	}
	rows := r.buildAndMirrorRemoteOffers("222", 222, offers)
	if len(rows) != 1 {
		t.Errorf("expected 1 row (good offer only), got %d", len(rows))
	}
	if len(rows) > 0 && rows[0].OfferID != "good-1" {
		t.Errorf("expected good-1, got %q", rows[0].OfferID)
	}
}
