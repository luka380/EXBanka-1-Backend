package handler_test

import (
	"context"
	"testing"

	stockpb "github.com/exbanka/contract/stockpb"
	"github.com/exbanka/stock-service/internal/model"
	"github.com/shopspring/decimal"
)

// TestLookupPeerOptionContract_FoundSellerSide verifies the RPC returns the
// stored SELLER-side (DEBIT) terms for a negotiationId this bank holds.
func TestLookupPeerOptionContract_FoundSellerSide(t *testing.T) {
	h, db, _, _ := newPeerOtcHandler(t)
	ctx := context.Background()

	row := &model.PeerOptionContract{
		CrossbankTxID:            "222:k-1",
		PostingIndex:             2,
		NegotiationRoutingNumber: 111,
		NegotiationID:            "neg-1",
		BuyerRoutingNumber:       222,
		BuyerID:                  "client-7",
		SellerRoutingNumber:      111,
		SellerID:                 "client-3",
		Ticker:                   "WMT",
		Quantity:                 10,
		StrikePrice:              decimal.RequireFromString("50"),
		Currency:                 "RSD",
		SettlementDate:           "2026-12-31T00:00:00+02:00",
		Direction:                "DEBIT",
		Status:                   "active",
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, err := h.LookupPeerOptionContract(ctx, &stockpb.LookupPeerOptionContractRequest{
		NegotiationRoutingNumber: 111,
		NegotiationId:            "neg-1",
	})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !resp.GetFound() {
		t.Fatalf("expected found=true")
	}
	if resp.GetSellerId() != "client-3" {
		t.Errorf("seller_id: %q", resp.GetSellerId())
	}
	if resp.GetTicker() != "WMT" {
		t.Errorf("ticker: %q", resp.GetTicker())
	}
	if resp.GetStrikePrice() != "50" {
		t.Errorf("strike_price: %q", resp.GetStrikePrice())
	}
	if resp.GetQuantity() != 10 {
		t.Errorf("quantity: %d", resp.GetQuantity())
	}
	if resp.GetCurrency() != "RSD" {
		t.Errorf("currency: %q", resp.GetCurrency())
	}
	if resp.GetSettlementDate() != "2026-12-31T00:00:00+02:00" {
		t.Errorf("settlement_date: %q", resp.GetSettlementDate())
	}
	if resp.GetStatus() != "active" {
		t.Errorf("status: %q", resp.GetStatus())
	}
}

// TestLookupPeerOptionContract_NotFound verifies found=false when this bank does
// not hold the seller-side row for the negotiationId (e.g. it holds the buyer
// side only, or knows nothing of the negotiation).
func TestLookupPeerOptionContract_NotFound(t *testing.T) {
	h, db, _, _ := newPeerOtcHandler(t)
	ctx := context.Background()

	// Only a CREDIT (buyer-side) row exists — the seller-side lookup must miss.
	row := &model.PeerOptionContract{
		CrossbankTxID:            "222:k-2",
		PostingIndex:             3,
		NegotiationRoutingNumber: 111,
		NegotiationID:            "neg-2",
		BuyerRoutingNumber:       111,
		BuyerID:                  "client-1",
		SellerRoutingNumber:      222,
		SellerID:                 "client-9",
		Ticker:                   "WMT",
		Quantity:                 5,
		StrikePrice:              decimal.RequireFromString("50"),
		Currency:                 "RSD",
		SettlementDate:           "2026-12-31T00:00:00+02:00",
		Direction:                "CREDIT",
		Status:                   "active",
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, err := h.LookupPeerOptionContract(ctx, &stockpb.LookupPeerOptionContractRequest{
		NegotiationRoutingNumber: 111,
		NegotiationId:            "neg-2",
	})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if resp.GetFound() {
		t.Fatalf("expected found=false for a buyer-side-only negotiation")
	}
}
