# Options & Premium Tax Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the 15% capital-gains tax to OTC option premiums, exercises, and expiries using a resolution-month / exercise-time taxation model, with bank (Profit Banke) exemption.

**Architecture:** All changes live in `stock-service` (intra-bank) plus its cross-bank SI-TX handler. Saga edits only change existing step *bodies* (never add/remove steps) so crash-recovery is unaffected. Tax writes are best-effort and never block money movement. Buyer's acquired-share cost basis steps up to market at exercise to prevent double taxation.

**Tech Stack:** Go, GORM (Postgres prod / SQLite tests), shopspring/decimal, the in-house saga framework (`contract/shared/saga`), gRPC.

Reference spec: `docs/superpowers/specs/2026-06-04-options-premium-tax-design.md`.

---

## File Structure

- `stock-service/internal/repository/tax_collection_repository.go` — add bank exclusion (Task 1).
- `stock-service/internal/repository/capital_gain_repository.go` — add `CreateIdempotent` (Task 3).
- `stock-service/internal/service/interfaces.go` — add `CreateIdempotent` to `CapitalGainRepo` (Task 3).
- `stock-service/internal/service/otc_accept_saga.go` — neutralize buyer premium step (Task 2).
- `stock-service/internal/service/otc_exercise_saga.go` — buyer exercise gain + basis step-up (Task 4).
- `stock-service/internal/service/otc_expiry_cron.go` — buyer expiry loss row (Task 5).
- `stock-service/cmd/main.go` — wire capital-gain repo into expiry cron (Task 5).
- `stock-service/internal/handler/peer_otc_grpc_handler.go` + `model/peer_option_contract.go` — cross-bank (Task 6).
- `stock-service/internal/service/tax_cutover.go` (new) + `cmd/main.go` — one-time cleanup (Task 7).
- `Specification.md`, `VERSION`, `docs/api/REST_API_v3.md` — docs (Task 8).
- `test-app/workflows/wf_option_tax_test.go` (new) — integration (Task 9).

---

## Task 1: Bank exemption in tax collection (lowest-risk, isolated)

**Files:**
- Modify: `stock-service/internal/repository/tax_collection_repository.go` (in `ListOwnersWithGains`, ~line 137 after `baseQuery` is built)
- Test: `stock-service/internal/repository/tax_collection_repository_test.go` (create if absent, else append)

- [ ] **Step 1: Write the failing test**

Append (or create file with package header `package repository` + imports `testing`, `gorm sqlite`, `decimal`, `model`):

```go
func TestListOwnersWithGains_ExcludesBankOwners(t *testing.T) {
	db := newTaxRepoDB(t) // helper opening sqlite :memory: and AutoMigrate(&model.CapitalGain{}, &model.TaxCollection{}, &model.Holding{})
	repo := NewTaxCollectionRepository(db)
	cgRepo := NewCapitalGainRepository(db)

	clientID := uint64(5)
	// Client gain — should be returned.
	if err := cgRepo.Create(&model.CapitalGain{
		OwnerType: model.OwnerClient, OwnerID: &clientID, OTC: true, SecurityType: "option",
		Ticker: "AAPL", Quantity: 50, BuyPricePerUnit: decimal.Zero, SellPricePerUnit: decimal.NewFromInt(23),
		TotalGain: decimal.NewFromInt(1150), Currency: "USD", AccountID: 11, TaxYear: 2026, TaxMonth: 6,
	}); err != nil {
		t.Fatalf("seed client: %v", err)
	}
	// Bank gain — must NOT be returned (Profit Banke exemption).
	if err := cgRepo.Create(&model.CapitalGain{
		OwnerType: model.OwnerBank, OwnerID: nil, OTC: true, SecurityType: "option",
		Ticker: "AAPL", Quantity: 50, BuyPricePerUnit: decimal.Zero, SellPricePerUnit: decimal.NewFromInt(23),
		TotalGain: decimal.NewFromInt(1150), Currency: "USD", AccountID: 12, TaxYear: 2026, TaxMonth: 6,
	}); err != nil {
		t.Fatalf("seed bank: %v", err)
	}

	rows, _, err := repo.ListOwnersWithGains(2026, 6, TaxFilter{Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range rows {
		if r.OwnerType == string(model.OwnerBank) {
			t.Fatalf("bank owner must be excluded from tax collection, got %+v", r)
		}
	}
	if len(rows) != 1 || rows[0].OwnerType != string(model.OwnerClient) {
		t.Fatalf("expected exactly the client owner, got %+v", rows)
	}
}
```

If `newTaxRepoDB` does not exist in this package, add it:

```go
func newTaxRepoDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&model.CapitalGain{}, &model.TaxCollection{}, &model.Holding{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd stock-service && go test ./internal/repository/ -run TestListOwnersWithGains_ExcludesBankOwners -v`
Expected: FAIL — bank owner currently returned.

- [ ] **Step 3: Add the bank exclusion**

In `ListOwnersWithGains`, immediately after the `baseQuery := r.db.Table("capital_gains cg")...Where("cg.tax_year = ? AND cg.tax_month = ? AND cg.tax_collection_id IS NULL", year, month)` chain (before the `if filter.UserType != ""` block), add:

```go
	// Profit Banke exemption: bank-owned gains (actuary trading on behalf of
	// the bank — premiums, option exercise, dividends, stock) are never taxed;
	// the profit stays with the bank. Same rule dividends require.
	// Spec: docs/superpowers/specs/2026-06-04-options-premium-tax-design.md §3.2
	baseQuery = baseQuery.Where("cg.owner_type = ?", string(model.OwnerClient))
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd stock-service && go test ./internal/repository/ -run TestListOwnersWithGains_ExcludesBankOwners -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add stock-service/internal/repository/tax_collection_repository.go stock-service/internal/repository/tax_collection_repository_test.go
git commit -m "feat(tax): exempt bank-owned gains from collection (Profit Banke)"
```

---

## Task 2: Neutralize buyer premium row at accept (resolution-month model)

**Files:**
- Modify: `stock-service/internal/service/otc_accept_saga.go:362-394` (the `StepRecordBuyerPremiumCost` step)
- Test: `stock-service/internal/service/otc_accept_saga_test.go`

- [ ] **Step 1: Write the failing test**

Find the existing accept-saga happy-path test (it constructs an `OTCOfferService` with a fake `capitalGainRepo` and runs `Accept`). Add an assertion-focused test. If the existing fake capital-gain repo records created rows in a slice, assert no buyer-premium (`TotalGain` negative, `SecurityType=="option"`) row exists. Example using a recording fake `recordingCGRepo` with field `created []*model.CapitalGain`:

```go
func TestAcceptSaga_NoBuyerPremiumRow(t *testing.T) {
	h := newAcceptSagaHarness(t) // existing helper that wires a recordingCGRepo
	_, err := h.svc.Accept(context.Background(), h.acceptInput())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	var sellerRows, buyerRows int
	for _, cg := range h.cgRepo.created {
		if cg.SecurityType != "option" {
			continue
		}
		if cg.TotalGain.IsNegative() {
			buyerRows++
		}
		if cg.TotalGain.IsPositive() {
			sellerRows++
		}
	}
	if buyerRows != 0 {
		t.Fatalf("buyer premium row must NOT be booked at accept (resolution-month model), got %d", buyerRows)
	}
	if sellerRows != 1 {
		t.Fatalf("seller premium row must still be booked at accept, got %d", sellerRows)
	}
}
```

> If no `newAcceptSagaHarness`/`recordingCGRepo` helper exists, reuse the construction from the nearest existing accept-saga test in the same file and add a minimal recording fake implementing `CapitalGainRepo` whose `Create` appends to `created`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd stock-service && go test ./internal/service/ -run TestAcceptSaga_NoBuyerPremiumRow -v`
Expected: FAIL — buyerRows == 1.

- [ ] **Step 3: Neutralize the step body**

Replace the `Forward` and `Backward` closures of the `StepRecordBuyerPremiumCost` step (otc_accept_saga.go:362-394) with no-ops, keeping the step in the chain (shape unchanged for recovery):

```go
		Add(saga.Step{
			Name: saga.StepRecordBuyerPremiumCost,
			// Resolution-month model (2026-06-04): the buyer's premium is no
			// longer booked as a capital-gain cost at accept. It is realised
			// when the option resolves — netted into the exercise gain
			// (otc_exercise_saga.go) or booked as a loss at expiry
			// (otc_expiry_cron.go). The seller's premium income is still taxed
			// at accept (StepRecordSellerPremiumGain). Step kept in place (no
			// shape change) so saga crash-recovery is unaffected.
			// Spec: docs/superpowers/specs/2026-06-04-options-premium-tax-design.md §3,§4 C1
			Forward:  func(ctx context.Context, _ *saga.State) error { return nil },
			Backward: func(ctx context.Context, _ *saga.State) error { return nil },
		}).
```

Remove now-unused locals if the compiler flags them (e.g. `buyerCG`, `buyerCGKey`, `buyerAccountID` if only used here). If `buyerAccountID`/`buyerCGKey` are still referenced elsewhere in the function, leave them.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd stock-service && go test ./internal/service/ -run 'TestAcceptSaga' -v`
Expected: PASS (new test + existing accept tests still green).

- [ ] **Step 5: Commit**

```bash
git add stock-service/internal/service/otc_accept_saga.go stock-service/internal/service/otc_accept_saga_test.go
git commit -m "feat(tax): stop booking buyer option premium at accept (resolution-month)"
```

---

## Task 3: Add idempotent capital-gain insert (for the expiry cron)

**Files:**
- Modify: `stock-service/internal/repository/capital_gain_repository.go` (after `Create`, ~line 43)
- Modify: `stock-service/internal/service/interfaces.go` (`CapitalGainRepo` interface, ~line 172)
- Test: `stock-service/internal/repository/capital_gain_repository_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestCreateIdempotent_NoDuplicateOnSameKey(t *testing.T) {
	db := newTaxRepoDB(t)
	repo := NewCapitalGainRepository(db)
	key := "expire-contract-99-buyer-premium-loss"
	cid := uint64(7)
	row := func() *model.CapitalGain {
		return &model.CapitalGain{
			OwnerType: model.OwnerClient, OwnerID: &cid, OTC: true, SecurityType: "option",
			Ticker: "AAPL", Quantity: 50, BuyPricePerUnit: decimal.Zero, SellPricePerUnit: decimal.Zero,
			TotalGain: decimal.NewFromInt(-1150), Currency: "USD", AccountID: 11, TaxYear: 2026, TaxMonth: 6,
			IdempotencyKey: &key,
		}
	}
	if err := repo.CreateIdempotent(row()); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := repo.CreateIdempotent(row()); err != nil {
		t.Fatalf("second insert must be a no-op, got: %v", err)
	}
	var n int64
	db.Model(&model.CapitalGain{}).Where("idempotency_key = ?", key).Count(&n)
	if n != 1 {
		t.Fatalf("expected exactly 1 row for key, got %d", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd stock-service && go test ./internal/repository/ -run TestCreateIdempotent -v`
Expected: FAIL — `CreateIdempotent` undefined.

- [ ] **Step 3: Implement `CreateIdempotent`**

In `capital_gain_repository.go`, add (import `gorm.io/gorm/clause` if not present):

```go
// CreateIdempotent inserts a capital-gain row, doing nothing on a conflict
// with an existing idempotency_key. Used by non-saga callers (the expiry cron)
// that may re-process the same contract after a partial failure. Rows without
// an idempotency_key are inserted normally (NULL keys never conflict).
func (r *CapitalGainRepository) CreateIdempotent(gain *model.CapitalGain) error {
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "idempotency_key"}},
		DoNothing: true,
	}).Create(gain).Error
}
```

In `interfaces.go`, add to the `CapitalGainRepo` interface:

```go
	CreateIdempotent(gain *model.CapitalGain) error
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd stock-service && go test ./internal/repository/ -run TestCreateIdempotent -v && cd stock-service && go build ./...`
Expected: PASS and build succeeds (interface addition compiles; recording fakes in service tests may need the method — add a no-op/append `CreateIdempotent` to any fake implementing `CapitalGainRepo`).

- [ ] **Step 5: Commit**

```bash
git add stock-service/internal/repository/capital_gain_repository.go stock-service/internal/service/interfaces.go stock-service/internal/repository/capital_gain_repository_test.go
git commit -m "feat(tax): add CreateIdempotent for capital gains (ON CONFLICT DO NOTHING)"
```

---

## Task 4: Buyer exercise gain + cost-basis step-up (intra-bank saga)

**Files:**
- Modify: `stock-service/internal/service/otc_exercise_saga.go` — pre-saga snapshot (~line 124-137 area), `buyerHolding.AveragePrice` (line 202), `StepRecordBuyerExerciseCost` body (line 360-377)
- Test: `stock-service/internal/service/otc_exercise_saga_guards_test.go`

- [ ] **Step 1: Write the failing test**

Add a test that runs `ExerciseContract` with market > strike and asserts the buyer row and basis. Mirror the existing guards-test harness (it wires accounts, holdings, reservations, capitalGainRepo, stockMeta). Use a stockMeta fake returning `Listing{Price: 250}` and a contract with strike 200, premium 1150, qty 50:

```go
func TestExerciseSaga_BuyerExerciseGain_AndBasisStepUp(t *testing.T) {
	h := newExerciseHarness(t) // existing helper; sets strike=200, qty=50
	h.contract.PremiumPaid = decimal.NewFromInt(1150)
	h.contract.PremiumCurrency = "USD"
	h.contract.StrikeCurrency = "USD"
	h.stockMeta.listing = &model.Listing{ID: 9, Price: decimal.NewFromInt(250)} // market

	if _, err := h.svc.ExerciseContract(context.Background(), h.exerciseInput()); err != nil {
		t.Fatalf("exercise: %v", err)
	}

	// Buyer option gain row: (250-200)*50 - 1150 = 1350
	var buyer *model.CapitalGain
	for _, cg := range h.cgRepo.created {
		if cg.SecurityType == "option" && cg.OTC {
			buyer = cg
		}
	}
	if buyer == nil {
		t.Fatal("expected a buyer exercise option capital-gain row")
	}
	if !buyer.TotalGain.Equal(decimal.NewFromInt(1350)) {
		t.Fatalf("buyer gain = %s, want 1350", buyer.TotalGain)
	}
	// Cost basis steps up to market (250), not strike (200).
	if !h.upsertedBuyerHolding.AveragePrice.Equal(decimal.NewFromInt(250)) {
		t.Fatalf("buyer holding basis = %s, want 250 (market)", h.upsertedBuyerHolding.AveragePrice)
	}
}
```

> If the harness names differ, adapt to the existing guards-test setup in the same file; the key fields to control are the contract (strike/qty/premium), the stockMeta listing price, and a way to read the upserted buyer holding (the holdingRepo fake's last upsert).

- [ ] **Step 2: Run test to verify it fails**

Run: `cd stock-service && go test ./internal/service/ -run TestExerciseSaga_BuyerExerciseGain -v`
Expected: FAIL — no buyer option row (step is a no-op) and basis == 200.

- [ ] **Step 3a: Snapshot market price pre-saga**

In `buildExerciseSaga`, near the `sellerCostBasis` snapshot block (after line 137), add a market-price snapshot:

```go
	// Snapshot the underlying's current market price for the buyer's
	// exercise-gain row ((market-strike)*qty - premium). Done pre-saga so a
	// lookup failure degrades to "skip the buyer tax row" rather than blocking
	// the exercise (money safety first). Market price is in the underlying's
	// exchange currency, which equals the strike currency for a same-stock
	// option. Spec §4 C2.
	var marketPrice decimal.Decimal
	marketPriceKnown := false
	if s.capitalGainRepo != nil && s.stockMeta != nil {
		if lst, lerr := s.stockMeta.GetListingBySecurityIDAndType(c.StockID, "stock"); lerr == nil && lst != nil && lst.Price.IsPositive() {
			marketPrice = lst.Price
			marketPriceKnown = true
		} else if lerr != nil {
			log.Printf("WARN: OTC exercise saga=%s: market price lookup failed (buyer exercise gain skipped, basis falls back to strike): %v", sagaID, lerr)
		}
	}
	buyerExerciseGainKey := fmt.Sprintf("%s:buyer-exercise-cg", sagaID)
```

- [ ] **Step 3b: Step up the buyer holding basis to market**

Change line 202 from `AveragePrice: c.StrikePrice,` to:

```go
		AveragePrice: func() decimal.Decimal {
			if marketPriceKnown {
				return marketPrice // basis step-up: buyer was taxed on (market-strike) at exercise
			}
			return c.StrikePrice // degraded fallback when market price unknown
		}(),
```

- [ ] **Step 3c: Fill the buyer exercise-cost step**

Replace the `StepRecordBuyerExerciseCost` `Forward`/`Backward` (line 360-377) with:

```go
		Add(saga.Step{
			Name: saga.StepRecordBuyerExerciseCost,
			// Resolution-month model (2026-06-04): tax the buyer at exercise on
			// (market - strike) * qty - premium, in the exercise month. The
			// premium is no longer booked at accept (see otc_accept_saga.go).
			// Basis stepped up to market on the credited holding so a later
			// sale does not re-tax (market - strike). Best-effort: skipped when
			// the market price is unknown. Spec §3.1, §4 C2.
			Forward: func(ctx context.Context, _ *saga.State) error {
				if s.capitalGainRepo == nil || !marketPriceKnown {
					return nil
				}
				premiumInStrike := c.PremiumPaid
				if c.PremiumCurrency != c.StrikeCurrency && s.exchange != nil {
					conv, cerr := s.exchange.Convert(ctx, &exchangepb.ConvertRequest{
						FromCurrency: c.PremiumCurrency, ToCurrency: c.StrikeCurrency, Amount: c.PremiumPaid.String(),
					})
					if cerr != nil {
						log.Printf("WARN: OTC exercise saga=%s: premium FX convert failed (buyer gain skipped): %v", sagaID, cerr)
						return nil
					}
					parsed, perr := decimal.NewFromString(conv.ConvertedAmount)
					if perr != nil {
						log.Printf("WARN: OTC exercise saga=%s: premium FX parse %q failed (buyer gain skipped): %v", sagaID, conv.ConvertedAmount, perr)
						return nil
					}
					premiumInStrike = parsed
				}
				gain := marketPrice.Sub(c.StrikePrice).Mul(decimal.NewFromInt(qty)).Sub(premiumInStrike)
				cg := &model.CapitalGain{
					OwnerType:        c.BuyerOwnerType,
					OwnerID:          c.BuyerOwnerID,
					OTC:              true,
					SecurityType:     "option",
					Ticker:           c.Ticker,
					Quantity:         qty,
					BuyPricePerUnit:  c.StrikePrice,
					SellPricePerUnit: marketPrice,
					TotalGain:        gain, // may be negative; reduces the buyer's month gain
					Currency:         c.StrikeCurrency,
					AccountID:        c.BuyerAccountID,
					TaxYear:          exercisedAt.Year(),
					TaxMonth:         int(exercisedAt.Month()),
					IdempotencyKey:   &buyerExerciseGainKey,
				}
				return s.capitalGainRepo.Create(cg)
			},
			Backward: func(ctx context.Context, _ *saga.State) error {
				if s.capitalGainRepo == nil {
					return nil
				}
				return s.capitalGainRepo.DeleteByIdempotencyKey(buyerExerciseGainKey)
			},
		}).
```

- [ ] **Step 4: Run tests (including rollback) to verify they pass**

Run: `cd stock-service && go test ./internal/service/ -run 'TestExerciseSaga' -v`
Expected: PASS. If a fault-injection rollback test exists for the exercise saga, confirm it still passes (the new Backward deletes the buyer row by key). If not present, add one that force-fails `StepMarkContractExercised` and asserts the buyer exercise CG row was deleted.

- [ ] **Step 5: Commit**

```bash
git add stock-service/internal/service/otc_exercise_saga.go stock-service/internal/service/otc_exercise_saga_guards_test.go
git commit -m "feat(tax): record buyer option exercise gain + step basis up to market"
```

---

## Task 5: Buyer expiry loss row (intra-bank cron)

**Files:**
- Modify: `stock-service/internal/service/otc_expiry_cron.go` (struct + `WithCapitalGains` + `expireContract`)
- Modify: `stock-service/cmd/main.go` (~line 851 `NewOTCExpiryCron(...)` chain)
- Test: `stock-service/internal/service/otc_expiry_cron_test.go`

- [ ] **Step 1: Write the failing test**

Add to the expiry cron test (the harness uses sqlite + real repos). Also add `&model.CapitalGain{}` to the `AutoMigrate` list in `newOTCExpiryDB`.

```go
func TestOTCExpiryCron_BooksBuyerPremiumLoss(t *testing.T) {
	db := newOTCExpiryDB(t)
	contractRepo := repository.NewOptionContractRepository(db)
	cgRepo := repository.NewCapitalGainRepository(db)
	cr := NewOTCExpiryCron(contractRepo, repository.NewOTCOfferRepository(db), nil, nil, 10, "02:00", nilRegistry()).
		WithCapitalGains(cgRepo)

	uid := uint64(7)
	c := &model.OptionContract{
		StockID: 42, Quantity: decimal.NewFromInt(10),
		StrikePrice: decimal.NewFromInt(150), PremiumPaid: decimal.NewFromInt(1150),
		PremiumCurrency: "USD", StrikeCurrency: "USD",
		SettlementDate: time.Now().Add(-24 * time.Hour),
		Status:         model.OptionContractStatusActive,
		BuyerOwnerType: model.OwnerClient, BuyerOwnerID: &uid,
		SellerOwnerType: model.OwnerBank, SellerOwnerID: nil,
		BuyerAccountID: 11, SellerAccountID: 12,
	}
	if err := contractRepo.Create(c); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := cr.expireContract(context.Background(), c); err != nil {
		t.Fatalf("expire: %v", err)
	}
	// Re-running must not duplicate the loss row (idempotent).
	c.Status = model.OptionContractStatusActive
	_ = cr.expireContract(context.Background(), c)

	var rows []model.CapitalGain
	db.Where("owner_type = ? AND security_type = ?", model.OwnerClient, "option").Find(&rows)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 buyer loss row (idempotent), got %d", len(rows))
	}
	if !rows[0].TotalGain.Equal(decimal.NewFromInt(-1150)) {
		t.Fatalf("buyer loss = %s, want -1150", rows[0].TotalGain)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd stock-service && go test ./internal/service/ -run TestOTCExpiryCron_BooksBuyerPremiumLoss -v`
Expected: FAIL — `WithCapitalGains` undefined.

- [ ] **Step 3: Implement cron wiring + loss row**

In `otc_expiry_cron.go`, add a field to the struct:

```go
	// capitalGains, when wired, books the buyer's lost-premium capital-gain
	// row at expiry (resolution-month model). nil disables (legacy tests).
	capitalGains CapitalGainRepo
```

Add the builder near `WithPeerContracts`:

```go
// WithCapitalGains wires the capital-gain repo so contract expiry books the
// buyer's lost-premium loss row (TotalGain = -premium) in the expiry month.
// Spec §4 C3.
func (cr *OTCExpiryCron) WithCapitalGains(repo CapitalGainRepo) *OTCExpiryCron {
	cr.capitalGains = repo
	return cr
}
```

In `expireContract`, add the loss-row write **before** the status flip (`c.Status = ...Expired`), so a crash between insert and flip re-runs safely:

```go
	// Resolution-month model: the buyer's premium is realised as a loss at
	// expiry, reducing their capital gain for the expiry month. Seller keeps
	// the premium (already taxed at accept). Idempotent by contract-scoped key
	// so a re-run (status-flip failure) does not double-book. Spec §3.1, §4 C3.
	if cr.capitalGains != nil && c.PremiumPaid.IsPositive() {
		now := time.Now().UTC()
		lossKey := fmt.Sprintf("expire-contract-%d-buyer-premium-loss", c.ID)
		loss := &model.CapitalGain{
			OwnerType:        c.BuyerOwnerType,
			OwnerID:          c.BuyerOwnerID,
			OTC:              true,
			SecurityType:     "option",
			Ticker:           c.Ticker,
			Quantity:         c.Quantity.IntPart(),
			BuyPricePerUnit:  decimal.Zero,
			SellPricePerUnit: decimal.Zero,
			TotalGain:        c.PremiumPaid.Neg(),
			Currency:         c.PremiumCurrency,
			AccountID:        c.BuyerAccountID,
			TaxYear:          now.Year(),
			TaxMonth:         int(now.Month()),
			IdempotencyKey:   &lossKey,
		}
		if err := cr.capitalGains.CreateIdempotent(loss); err != nil {
			return err // do not flip status if the loss row failed; retry next pass
		}
	}
```

Add imports `fmt` and `github.com/shopspring/decimal` to the file if missing.

In `cmd/main.go`, append `.WithCapitalGains(capitalGainRepo)` to the `otcExpiry` builder chain (~line 851-853):

```go
	otcExpiry := service.NewOTCExpiryCron(optionContractRepo, otcOfferRepo, holdingReservationSvc, producer, cfg.OTCExpiryBatchSize, cfg.OTCExpiryCronUTC, cronRegistry).
		WithOutbox(ob, db).
		WithPeerContracts(peerOptionRepo).
		WithCapitalGains(capitalGainRepo)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd stock-service && go test ./internal/service/ -run TestOTCExpiryCron -v && go build ./...`
Expected: PASS and build succeeds.

- [ ] **Step 5: Commit**

```bash
git add stock-service/internal/service/otc_expiry_cron.go stock-service/internal/service/otc_expiry_cron_test.go stock-service/cmd/main.go
git commit -m "feat(tax): book buyer lost-premium loss at OTC contract expiry"
```

---

## Task 3 — SUPERSEDED

`CapitalGainRepository.Create` already does `ON CONFLICT (idempotency_key) DO NOTHING` when a key is set (capital_gain_repository.go:30-38), so no separate `CreateIdempotent` is needed. The expiry cron (Task 5) calls `Create` with a deterministic key for free idempotency. No interface/mock changes.

## Task 6: Cross-bank (SI-TX) buyer taxation — DEFERRED (documented gap)

After code inspection this is **deferred**, not implemented: the frozen SI-TX `OptionDescription` wire carries no premium and the peer handler has no price resolver, so `(market−strike)×qty − premium` is uncomputable on the buyer's bank. Recorded in `docs/Bugs.txt` §"Cohort-dependent TODOs" item 5 and the spec §5. Cross-bank sellers remain taxed (unchanged); cross-bank buyers are taxed via eventual stock sale as before (no regression). No code/saga change.

### (original best-effort plan, retained for the future unblock path)

> **Higher risk. Implement and verify intra-bank (Tasks 1-5) first.** Tax writes here must NEVER return an error that fails the SI-TX commit — log + skip on any missing data. Confirm exact helper signatures (`ExerciseBuyerCreditForPeerOption`, the accept-time premium leg) before editing; if the premium is not recoverable locally, take the documented fallback (record only `(market-strike)*qty`, log the omitted premium).

**Files:**
- Modify: `stock-service/internal/model/peer_option_contract.go` (add `PremiumPaid`, `PremiumCurrency`)
- Modify: `stock-service/internal/handler/peer_otc_grpc_handler.go` (accept path populates premium; `recordOptionExercise` DirectionCredit books buyer gain + market basis)
- Modify: `stock-service/internal/service/otc_expiry_cron.go` (`expirePeerContract` books buyer loss on CREDIT side)
- Test: `stock-service/internal/handler/peer_otc_grpc_handler_test.go`, `stock-service/internal/service/otc_expiry_cron_test.go`

- [ ] **Step 1: Add premium columns to the peer contract**

In `peer_option_contract.go`, after `Currency`:

```go
	// PremiumPaid/PremiumCurrency: the option premium the buyer paid at accept,
	// persisted locally on BOTH banks' rows so the buyer's bank can compute the
	// exercise gain ((market-strike)*qty - premium) and the expiry loss
	// (-premium) without a SI-TX wire change (OptionDescription carries no
	// premium). Zero on legacy rows. Spec §5 X1.
	PremiumPaid     decimal.Decimal `gorm:"type:numeric(20,8);not null;default:0" json:"premium_paid"`
	PremiumCurrency string          `gorm:"size:8;not null;default:''" json:"premium_currency"`
```

- [ ] **Step 2: Populate premium at accept (locate + write test first)**

Find where `peer_option_contracts` rows are created at accept (`RecordOptionContract` form/accept path) and where the accept-time premium money leg amount is available. Write a handler test asserting a formed peer contract carries `PremiumPaid`/`PremiumCurrency` from the accept posting, then populate the two fields when constructing the row. If the premium leg is not available in that handler, set them from the negotiation/contract terms already in scope. (Concrete field source confirmed during implementation; the row construction site is the single place to set them.)

- [ ] **Step 3: Book buyer exercise gain on the buyer's bank**

In `recordOptionExercise`, `case contractsitx.DirectionCredit`, after `ExerciseBuyerCreditForPeerOption(...)` succeeds and before the status return, add a best-effort buyer gain (market via local listing lookup by ticker through the handler's listing/stock resolver; premium from the contract):

```go
		// Resolution-month model: tax the buyer at cross-bank exercise on
		// (market-strike)*qty - premium. Best-effort: any missing datum logs
		// and skips — a tax-row gap must never fail settlement. Spec §5 X2.
		if h.capitalGainRepo != nil {
			if market, ok := h.currentMarketPrice(contract.Ticker); ok {
				gain := market.Sub(contract.StrikePrice).Mul(decimal.NewFromInt(contract.Quantity)).Sub(contract.PremiumPaid)
				cg := &model.CapitalGain{
					OwnerType: ownerType, OwnerID: ownerID, OTC: true, SecurityType: "option",
					Ticker: contract.Ticker, Quantity: contract.Quantity,
					BuyPricePerUnit: contract.StrikePrice, SellPricePerUnit: market,
					TotalGain: gain, Currency: contract.Currency, TaxYear: time.Now().Year(), TaxMonth: int(time.Now().Month()),
				}
				if cgErr := h.capitalGainRepo.Create(cg); cgErr != nil {
					log.Printf("WARN: peer-option %d exercise: buyer gain CG create failed (settlement unaffected): %v", contract.ID, cgErr)
				}
			} else {
				log.Printf("WARN: peer-option %d exercise: market price unavailable, buyer exercise gain not recorded", contract.ID)
			}
		}
```

Add a small `currentMarketPrice(ticker string) (decimal.Decimal, bool)` helper on the handler backed by the existing listing repo (resolve listing by ticker → `Price`). If no listing resolver is wired, return `(zero, false)`.

- [ ] **Step 4: Step up cross-bank buyer basis to market**

`ExerciseBuyerCreditForPeerOption(ctx, contract.ID, ownerType, ownerID, ticker, qty, contract.StrikePrice)` passes strike as the average price (`:1435`). Pass the market price instead when known (thread it through, defaulting to strike when unknown). Update the helper signature if needed; keep strike as the fallback.

- [ ] **Step 5: Cross-bank expiry loss on the buyer's bank**

In `expirePeerContract`, on `c.Direction == "CREDIT"` (buyer side), book a best-effort `-premium` loss via `cr.capitalGains.CreateIdempotent` keyed `fmt.Sprintf("expire-peer-contract-%d-buyer-premium-loss", c.ID)`, using `c.PremiumPaid`/`c.PremiumCurrency`. Skip when `cr.capitalGains == nil` or premium is zero. Buyer owner parsed via the same `parseSellerOwner(c.BuyerID)` used elsewhere.

- [ ] **Step 6: Tests + run**

Add handler tests for the DirectionCredit gain (market>strike) and a no-listing skip (asserts no error, no row). Add an expiry test for the CREDIT-side loss. Run:
`cd stock-service && go test ./internal/handler/ ./internal/service/ -run 'PeerOption|OTCExpiry' -v && go build ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add stock-service/internal/model/peer_option_contract.go stock-service/internal/handler/peer_otc_grpc_handler.go stock-service/internal/service/otc_expiry_cron.go stock-service/internal/handler/peer_otc_grpc_handler_test.go stock-service/internal/service/otc_expiry_cron_test.go
git commit -m "feat(tax): cross-bank buyer option exercise gain + expiry loss (best-effort)"
```

---

## Task 7: One-time cutover cleanup (avoid double-counting legacy premium rows)

**Files:**
- Create: `stock-service/internal/service/tax_cutover.go`
- Modify: `stock-service/cmd/main.go` (call once at startup, after repos are built)
- Test: `stock-service/internal/service/tax_cutover_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestCleanupLegacyBuyerPremiumRows(t *testing.T) {
	db := newOTCExpiryDB(t) // migrates OptionContract + (add) CapitalGain
	_ = db.AutoMigrate(&model.CapitalGain{})
	// Active contract with a legacy buyer-premium row (uncollected) -> deleted.
	cid := uint64(3)
	db.Create(&model.OptionContract{ID: 100, Status: model.OptionContractStatusActive, OfferID: 1,
		BuyerOwnerType: model.OwnerClient, BuyerOwnerID: &cid, SellerOwnerType: model.OwnerBank,
		StockID: 1, Quantity: decimal.NewFromInt(1), StrikePrice: decimal.NewFromInt(1), PremiumPaid: decimal.NewFromInt(1),
		PremiumCurrency: "USD", StrikeCurrency: "USD", SettlementDate: time.Now().Add(48 * time.Hour)})
	legacy := decimal.NewFromInt(-1150)
	db.Create(&model.CapitalGain{OwnerType: model.OwnerClient, OwnerID: &cid, OTC: true, SecurityType: "option",
		Ticker: "AAPL", Quantity: 50, TotalGain: legacy, Currency: "USD", AccountID: 11, TaxYear: 2026, TaxMonth: 5})

	n, err := CleanupLegacyBuyerPremiumRows(db)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row deleted, got %d", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd stock-service && go test ./internal/service/ -run TestCleanupLegacyBuyerPremiumRows -v`
Expected: FAIL — `CleanupLegacyBuyerPremiumRows` undefined.

- [ ] **Step 3: Implement the cleanup**

Create `tax_cutover.go`:

```go
package service

import (
	"github.com/exbanka/stock-service/internal/model"
	"gorm.io/gorm"
)

// CleanupLegacyBuyerPremiumRows deletes uncollected buyer option-premium COST
// rows (TotalGain < 0, SecurityType='option', OTC) that belong to option
// contracts still ACTIVE. Under the resolution-month model the buyer's premium
// is booked at exercise/expiry, so these accept-time rows would otherwise
// double-count. Idempotent and safe to run on every startup. Already-collected
// rows (tax_collection_id NOT NULL) are never touched.
// Spec §6.
func CleanupLegacyBuyerPremiumRows(db *gorm.DB) (int64, error) {
	sub := db.Model(&model.OptionContract{}).
		Select("buyer_owner_id").
		Where("status = ?", model.OptionContractStatusActive).
		Where("buyer_owner_id IS NOT NULL")
	res := db.Where("security_type = ? AND otc = ? AND total_gain < 0 AND tax_collection_id IS NULL", "option", true).
		Where("owner_type = ?", string(model.OwnerClient)).
		Where("owner_id IN (?)", sub).
		Delete(&model.CapitalGain{})
	return res.RowsAffected, res.Error
}
```

In `cmd/main.go`, after repos/db are ready (near the other one-time startup steps), call:

```go
	if n, err := service.CleanupLegacyBuyerPremiumRows(db); err != nil {
		log.Printf("WARN: legacy buyer-premium cleanup failed: %v", err)
	} else if n > 0 {
		log.Printf("tax cutover: removed %d legacy buyer-premium capital-gain rows", n)
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd stock-service && go test ./internal/service/ -run TestCleanupLegacyBuyerPremiumRows -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add stock-service/internal/service/tax_cutover.go stock-service/internal/service/tax_cutover_test.go stock-service/cmd/main.go
git commit -m "chore(tax): one-time cleanup of legacy buyer-premium rows on active contracts"
```

---

## Task 8: Docs, spec, version, lint

**Files:**
- Modify: `Specification.md` (business rules §21, capital-gain entity §18)
- Modify: `VERSION` and `api-gateway/internal/version/version.go`
- Modify: `docs/api/REST_API_v3.md` (only if a tax response shape changed — confirm; likely none)

- [ ] **Step 1: Update `Specification.md`**

Add to the business-rules section: the option premium tax (seller taxed 15% at accept), exercise tax (buyer taxed 15% × ((market−strike)×qty − premium) at exercise, basis steps up to market), expiry (buyer −premium loss in expiry month, seller none), and the bank (Profit Banke) exemption for actuary-on-behalf-of-bank trades. Note the resolution-month timing.

- [ ] **Step 2: Bump `VERSION` (MINOR)**

Read current `VERSION`; bump MINOR, reset PATCH (e.g. `1.7.3` → `1.8.0`). Set the same string in `api-gateway/internal/version/version.go` `var Version`.

- [ ] **Step 3: Lint + full service test**

Run: `cd stock-service && golangci-lint run ./... && go test ./...`
Expected: zero new lint warnings; all tests pass.

- [ ] **Step 4: Commit**

```bash
git add Specification.md VERSION api-gateway/internal/version/version.go docs/api/REST_API_v3.md
git commit -m "docs(tax): document option premium/exercise/expiry tax + bank exemption; bump VERSION"
```

---

## Task 9: Integration workflow test

**Files:**
- Create: `test-app/workflows/wf_option_tax_test.go`

- [ ] **Step 1: Write the integration test**

Using `test-app/workflows/helpers_test.go` helpers, drive a full OTC option lifecycle against the running stack:
1. Seller lists an option; buyer accepts (premium paid). Assert seller's monthly gain reflects `+premium`; buyer's reflects **no** premium row yet.
2. Buyer exercises with market > strike. Assert a buyer option gain row of `(market−strike)×qty − premium`.
3. Trigger monthly tax collection (admin RPC). Assert buyer account debited `15% × ((market−strike)×qty − premium)` and seller `15% × premium`.
4. Actuary-on-behalf-of-bank variant: assert **zero** tax collected (Profit Banke).
5. Separate flow: accept → let expire → collection. Assert buyer's month gain reduced by the premium.

Use the existing tax-collection trigger + account-balance helpers; assert on response bodies and balance deltas (not just status codes), per the testing requirement.

- [ ] **Step 2: Run the integration suite**

Run the workflow suite per `docs/superpowers/specs/2026-04-04-comprehensive-testing-design.md` (e.g. `cd test-app && go test ./workflows/ -run WfOptionTax -v` against a running stack).
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add test-app/workflows/wf_option_tax_test.go
git commit -m "test(tax): integration coverage for option premium/exercise/expiry tax + bank exemption"
```

---

## Self-Review notes

- **Spec coverage:** seller premium (existing, asserted Task 2/9) · buyer exercise (Task 4) · buyer expiry loss (Task 5) · bank exemption (Task 1) · cross-bank (Task 6) · resolution-month timing (Tasks 2/4/5) · cutover (Task 7) · basis step-up no-double-tax (Task 4) · docs/version (Task 8). All spec sections map to a task.
- **Saga safety:** Tasks 2 & 4 only change existing step bodies; no step added/removed → recovery shape preserved. Backward closures delete-by-key. Pre-saga snapshots fail before money moves only where safe; tax-row failures degrade to log+skip.
- **Idempotency:** expiry/cutover use `CreateIdempotent` / deterministic keys; cron inserts loss before status flip.
- **Type consistency:** `CapitalGainRepo` gains `CreateIdempotent` (Task 3) used by the cron (Task 5); `marketPriceKnown`/`marketPrice`/`buyerExerciseGainKey` defined in Task 4 Step 3a and used in 3b/3c.
```