package handler

import (
	"context"
	"testing"
	"time"

	stockpb "github.com/exbanka/contract/stockpb"
	"github.com/exbanka/stock-service/internal/model"
	"github.com/shopspring/decimal"
)

// TestGetPublicOptionOffers_BuyInitiated_NotPublished asserts that a
// buy_initiated OTC option listing is NEVER exposed to peers via the
// peer-facing GET /api/v3/public-option-offers endpoint.
//
// The SI-TX bank-to-bank protocol's OTC discovery model is strictly
// SELLER-CENTRIC: §3 ("collecting all the Sellers"), §3.1 PublicStock
// (the catalog lists only `sellers`), and §3.2 ("This request is sent
// from a Buyer's bank to a Seller's bank") all assume the published party
// is a SELLER offering shares. A buy_initiated listing's poster is a
// BUYER wanting to acquire shares — there is no spec representation for
// it on the wire. Publishing one would mislabel the buyer-poster as a
// `sellerId`, inviting peers to bid against it and silently inverting the
// economic roles on accept/exercise. So buy_initiated listings are
// intra-bank only and must be filtered out of cross-bank discovery.
func TestGetPublicOptionOffers_BuyInitiated_NotPublished(t *testing.T) {
	now := time.Now().UTC()
	owner := uint64(9)
	reader := &fakeOTCOfferReader{rows: []model.OTCOffer{
		{
			ID:                 1,
			InitiatorOwnerType: model.OwnerClient,
			InitiatorOwnerID:   &owner,
			Direction:          model.OTCDirectionSellInitiated, // exposable
			Ticker:             "AAPL",
			Quantity:           decimal.NewFromInt(10),
			StrikePrice:        decimal.NewFromInt(150),
			Premium:            decimal.NewFromInt(5),
			SettlementDate:     now,
			CreatedAt:          now,
			Status:             model.OTCOfferStatusOpen,
		},
		{
			ID:                 2,
			InitiatorOwnerType: model.OwnerClient,
			InitiatorOwnerID:   &owner,
			Direction:          model.OTCDirectionBuyInitiated, // poster is a BUYER — must NOT be published
			Ticker:             "MSFT",
			Quantity:           decimal.NewFromInt(3),
			StrikePrice:        decimal.NewFromInt(300),
			Premium:            decimal.NewFromInt(8),
			SettlementDate:     now,
			CreatedAt:          now,
			Status:             model.OTCOfferStatusOpen,
		},
	}}

	h := (&PeerOTCGRPCHandler{ownRouting: 111}).WithOTCOfferReader(reader, nil)

	resp, err := h.GetPublicOptionOffers(context.Background(), &stockpb.GetPublicOptionOffersRequest{})
	if err != nil {
		t.Fatalf("GetPublicOptionOffers: %v", err)
	}

	// Only the sell_initiated offer (id 1) may surface.
	if got := len(resp.GetOffers()); got != 1 {
		t.Fatalf("expected exactly 1 published offer (buy_initiated skipped), got %d", got)
	}
	for _, row := range resp.GetOffers() {
		if row.GetOfferId().GetId() == "2" {
			t.Errorf("buy_initiated offer (id 2) must NOT be published to peers (seller-centric discovery)")
		}
		if row.GetDirection() == model.OTCDirectionBuyInitiated {
			t.Errorf("no published offer may carry direction=buy_initiated; got offer %s", row.GetOfferId().GetId())
		}
	}
	if got := resp.GetOffers()[0].GetOfferId().GetId(); got != "1" {
		t.Errorf("the one published offer should be the sell_initiated offer (id 1), got %q", got)
	}
}
