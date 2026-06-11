package repository

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/exbanka/stock-service/internal/model"
)

// TestMergeDuplicateOpenOffers_SumsAndConsumes verifies the startup migration
// that collapses pre-existing duplicate OPEN LOCAL offers sharing
// (initiator_owner_id, ticker, direction) into the oldest row (summing
// quantity) and marks the rest consumed — the precondition for the partial
// unique index ux_otc_offer_open_owner_ticker_dir.
func TestMergeDuplicateOpenOffers_SumsAndConsumes(t *testing.T) {
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.OTCOffer{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewOTCOfferRepository(db)
	uid := uint64(7)
	mk := func(qty int64) *model.OTCOffer {
		return &model.OTCOffer{
			InitiatorOwnerType: model.OwnerClient, InitiatorOwnerID: &uid,
			Direction: model.OTCDirectionSellInitiated, StockID: 1, Ticker: "OPK",
			Quantity: decimal.NewFromInt(qty), Status: model.OTCOfferStatusOpen, Local: true,
			LastModifiedByPrincipalType: "client", LastModifiedByPrincipalID: 7,
		}
	}
	require.NoError(t, db.Create(mk(5)).Error)
	require.NoError(t, db.Create(mk(70)).Error)

	n, err := repo.MergeDuplicateOpenOffers()
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	var open []model.OTCOffer
	require.NoError(t, db.Where("status = ?", model.OTCOfferStatusOpen).Find(&open).Error)
	require.Len(t, open, 1)
	require.True(t, open[0].Quantity.Equal(decimal.NewFromInt(75)), "merged qty = 5+70")

	// Re-read the kept row directly to confirm the column update actually
	// persisted (RowsAffected == 1 path, no silent version-WHERE no-op).
	kept, err := repo.GetByID(open[0].ID)
	require.NoError(t, err)
	require.True(t, kept.Quantity.Equal(decimal.NewFromInt(75)))
	require.Equal(t, model.OTCOfferStatusOpen, kept.Status)

	// Idempotent: a second run is a no-op.
	n2, err := repo.MergeDuplicateOpenOffers()
	require.NoError(t, err)
	require.Equal(t, int64(0), n2)
}

// TestMergeDuplicateOpenOffers_CollapsesPending proves the migration now also
// collapses pre-existing PENDING (and COUNTERED) local duplicates, not just the
// status='open' ones. New local offers are created PENDING, so before the
// widening the merge was a no-op against the real-world duplicate population.
func TestMergeDuplicateOpenOffers_CollapsesPending(t *testing.T) {
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.OTCOffer{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewOTCOfferRepository(db)
	uid := uint64(7)
	mk := func(qty int64, status string) *model.OTCOffer {
		return &model.OTCOffer{
			InitiatorOwnerType: model.OwnerClient, InitiatorOwnerID: &uid,
			Direction: model.OTCDirectionSellInitiated, StockID: 1, Ticker: "OPK",
			Quantity: decimal.NewFromInt(qty), Status: status, Local: true,
			LastModifiedByPrincipalType: "client", LastModifiedByPrincipalID: 7,
		}
	}
	require.NoError(t, db.Create(mk(5, model.OTCOfferStatusPending)).Error)
	require.NoError(t, db.Create(mk(70, model.OTCOfferStatusPending)).Error)

	n, err := repo.MergeDuplicateOpenOffers()
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	// The oldest PENDING row survives carrying the summed quantity; the other
	// is consumed.
	var pending []model.OTCOffer
	require.NoError(t, db.Where("status = ?", model.OTCOfferStatusPending).Find(&pending).Error)
	require.Len(t, pending, 1)
	require.True(t, pending[0].Quantity.Equal(decimal.NewFromInt(75)), "merged qty = 5+70")

	var consumed int64
	require.NoError(t, db.Model(&model.OTCOffer{}).
		Where("status = ?", model.OTCOfferStatusConsumed).Count(&consumed).Error)
	require.Equal(t, int64(1), consumed)
}

// otcOpenOfferUniqueIndexDDL mirrors the partial unique index built in
// stock-service/cmd/main.go (ux_otc_offer_open_owner_ticker_dir). The test
// creates it by hand so it can prove the index — not just the service pre-check
// — rejects a duplicate open offer. sqlite supports a partial UNIQUE index with
// a WHERE clause containing IN (...).
const otcOpenOfferUniqueIndexDDL = `CREATE UNIQUE INDEX IF NOT EXISTS ux_otc_offer_open_owner_ticker_dir
	ON otc_offers (initiator_owner_id, ticker, direction)
	WHERE status IN ('open','PENDING','COUNTERED') AND local = true AND initiator_owner_id IS NOT NULL`

// TestCreate_PartialUniqueIndex_BackstopsDuplicateOpen proves the DB partial
// unique index is the authoritative backstop for the one-open-offer-per
// (owner,ticker,direction) invariant: a second PENDING local offer sharing the
// key is rejected at INSERT time even when nothing called the service pre-check,
// and IsUniqueViolation classifies the error.
func TestCreate_PartialUniqueIndex_BackstopsDuplicateOpen(t *testing.T) {
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.OTCOffer{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	require.NoError(t, db.Exec(otcOpenOfferUniqueIndexDDL).Error)

	repo := NewOTCOfferRepository(db)
	uid := uint64(7)
	mk := func() *model.OTCOffer {
		return &model.OTCOffer{
			InitiatorOwnerType: model.OwnerClient, InitiatorOwnerID: &uid,
			Direction: model.OTCDirectionSellInitiated, StockID: 1, Ticker: "OPK",
			Quantity: decimal.NewFromInt(10), Status: model.OTCOfferStatusPending, Local: true,
			LastModifiedByPrincipalType: "client", LastModifiedByPrincipalID: 7,
		}
	}

	// First PENDING local sell offer (owner 7, OPK, sell_initiated): OK.
	require.NoError(t, repo.Create(mk()))

	// A SECOND identical-key PENDING local offer inserted DIRECTLY via the repo
	// (bypassing the service pre-check) must violate the partial unique index.
	err := repo.Create(mk())
	require.Error(t, err)
	require.True(t, IsUniqueViolation(err), "expected a unique-constraint violation, got %v", err)
}

// TestIsUniqueViolation_Classification covers the cross-driver classifier.
func TestIsUniqueViolation_Classification(t *testing.T) {
	require.False(t, IsUniqueViolation(nil))
	require.False(t, IsUniqueViolation(errors.New("some other error")))
	require.True(t, IsUniqueViolation(errors.New("UNIQUE constraint failed: otc_offers.ticker")))
	require.True(t, IsUniqueViolation(errors.New("constraint failed: UNIQUE (otc_offers.ticker)")))
	require.True(t, IsUniqueViolation(errors.New(`duplicate key value violates unique constraint "ux_otc_offer_open_owner_ticker_dir"`)))
}
