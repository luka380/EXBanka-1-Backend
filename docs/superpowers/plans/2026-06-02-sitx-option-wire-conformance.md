# SI-TX OTC Option Wire Conformance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the SI-TX OTC **option** legs byte-conformant to the spec — reshape `OptionDescription` to nested `stock`/`pricePerUnit` (drop our `intent`), and re-model exercise from intent-tagged OPTION-asset markers to the spec's OPTION-pseudo-account + STOCK + MONAS form — without changing any internal saga/settlement behavior.

**Architecture:** Translation-layer. Internal saga, `peer_option_contracts`, holding reservations, and `RecordOptionContract` settlement stay unchanged. We change only: (1) the `OptionDescription` JSON struct, (2) the accept/exercise posting *builders* in stock-service, (3) the receiver *executor* in transaction-service to recognize STOCK assets and OPTION-pseudo-account legs and route them to the existing settlement calls, and (4) supply `intent` internally instead of over the wire. Spec authority: `docs/A protocol for bank-to-bank asset exchange.htm`; design: `docs/superpowers/specs/2026-06-02-sitx-option-wire-conformance-design.md`.

**Tech Stack:** Go workspace monorepo; gRPC/protobuf (`contract/`); `shopspring/decimal`; GORM/Postgres; the `contract/sitx` wire types + `mapping.go` translation layer.

---

## Background facts the implementer needs

- **Sign/direction convention** (`contract/sitx/mapping.go`): spec **negative** amount = credit = asset *leaves* → internal `DEBIT`; spec **positive** = debit = asset *arrives* → internal `CREDIT`. The wire builders set internal `Direction`; `InternalPostingToSpec` converts to signed amounts.
- **`mapping.go` already handles** `AccountType` ∈ {PERSON, ACCOUNT, OPTION} and `AssetType` ∈ {MONAS, STOCK, OPTION} generically — no change needed there.
- **Constants** (in `contract/sitx/types.go`): `AccountTypePerson/Account/Option`, `AssetTypeMonas/Stock/Option`, `DirectionDebit/Credit`, and NoVote reasons `NoVoteReasonOptionUsedOrExpired`, `NoVoteReasonOptionNegotiationNotFound`, `NoVoteReasonOptionAmountIncorrect` (defined, not yet emitted).
- **`intent` today**: carried inside the OPTION-asset JSON and read at COMMIT (`transaction-service/internal/handler/peer_tx_grpc_handler.go:430` → `od.Intent`) to drive `RecordOptionContract`'s accept-vs-exercise branch. After this plan: OPTION asset ⇒ always accept; exercise goes through the new pseudo-account path. The executor supplies `Intent` to `RecordOptionContract` (it is NOT read from the wire).
- **Run a single module's tests:** `cd <service> && go test ./... `. Lint: `cd <service> && golangci-lint run ./...`.
- **All commits target the `Development` branch.** End commit messages with the Co-Authored-By trailer used in this repo.

---

## Task 1: Reshape the `OptionDescription` wire struct

**Files:**
- Modify: `contract/sitx/otc_types.go:70-78`
- Modify: `contract/sitx/types.go` (add `MonetaryValue` type if absent — verify first)
- Test: `contract/sitx/otc_types_test.go`

- [ ] **Step 1: Verify whether `MonetaryValue` exists**

Run: `grep -rn "type MonetaryValue" contract/sitx/`
If it does NOT exist, add it in `contract/sitx/types.go` next to `MonetaryAsset`:

```go
// MonetaryValue is the §2.5 {amount, currency} money value used inside
// OptionDescription.pricePerUnit. Amount is a bare JSON number (DecimalNumber).
type MonetaryValue struct {
	Amount   DecimalNumber `json:"amount"`
	Currency string        `json:"currency"`
}
```

- [ ] **Step 2: Write the failing marshal test**

In `contract/sitx/otc_types_test.go` add:

```go
func TestOptionDescriptionSpecShape(t *testing.T) {
	od := OptionDescription{
		NegotiationID:  ForeignBankId{RoutingNumber: 111, ID: "neg-1"},
		Stock:          StockDescription{Ticker: "WMT"},
		PricePerUnit:   MonetaryValue{Amount: DecimalNumber{decimal.RequireFromString("50")}, Currency: "RSD"},
		SettlementDate: "2026-12-31T00:00:00+02:00",
		Amount:         10,
	}
	got, err := json.Marshal(od)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"negotiationId":{"routingNumber":111,"id":"neg-1"},"stock":{"ticker":"WMT"},"pricePerUnit":{"amount":50,"currency":"RSD"},"settlementDate":"2026-12-31T00:00:00+02:00","amount":10}`
	var g, w bytes.Buffer
	_ = json.Compact(&g, got)
	_ = json.Compact(&w, []byte(want))
	if g.String() != w.String() {
		t.Errorf("shape mismatch:\n got: %s\nwant: %s", g.String(), w.String())
	}
	// Ensure no leftover flat/intent fields leak.
	for _, bad := range []string{`"ticker"`, `"strikePrice"`, `"currency":`, `"intent"`} {
		if bytes.Contains(got, []byte(bad)) {
			t.Errorf("unexpected legacy field %s in %s", bad, got)
		}
	}
}
```

Add imports `bytes`, `encoding/json`, `github.com/shopspring/decimal` if missing.

- [ ] **Step 3: Run the test to verify it fails**

Run: `cd contract && go test ./sitx/ -run TestOptionDescriptionSpecShape -v`
Expected: compile error / FAIL (struct still has flat fields).

- [ ] **Step 4: Reshape the struct**

Replace `contract/sitx/otc_types.go:70-78` with:

```go
// OptionDescription is the §2.7.2 option asset payload (asset Type "OPTION").
// Spec shape: nested stock + pricePerUnit, no internal "intent" field — the
// transaction SHAPE (OPTION asset = accept; OPTION pseudo-account = exercise)
// encodes the operation, per the design doc.
type OptionDescription struct {
	NegotiationID  ForeignBankId    `json:"negotiationId"`
	Stock          StockDescription `json:"stock"`
	PricePerUnit   MonetaryValue    `json:"pricePerUnit"`
	SettlementDate string           `json:"settlementDate"`
	Amount         int64            `json:"amount"`
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd contract && go test ./sitx/ -run TestOptionDescriptionSpecShape -v`
Expected: PASS. (The `contract` module will NOT fully build yet — downstream files reference the old fields; that's fixed in Tasks 2–6. Do not commit until Task 6 restores the build of all modules; this task's commit happens at the end of Task 2 with the producers updated. Proceed to Task 2.)

---

## Task 2: Update the accept builder + executor reads to nested fields

**Files:**
- Modify: `stock-service/internal/handler/peer_otc_grpc_handler.go:738-745` (accept `optDesc`)
- Modify: `transaction-service/internal/sitx/posting_executor.go:96-103` (`optionDescriptionForCheck`), `:200-211` (validation pre-pass reads), `:256-269` (reserve reads)
- Modify: `transaction-service/internal/handler/peer_tx_grpc_handler.go:430` (`od.Intent` → constant)

- [ ] **Step 1: Update the accept builder `optDesc`**

In `stock-service/internal/handler/peer_otc_grpc_handler.go` replace the `optDesc :=` block at ~738:

```go
	optDesc := contractsitx.OptionDescription{
		NegotiationID:  contractsitx.ForeignBankId{RoutingNumber: h.ownRouting, ID: row.ForeignID},
		Stock:          contractsitx.StockDescription{Ticker: offer.Ticker},
		PricePerUnit:   contractsitx.MonetaryValue{Amount: contractsitx.DecimalNumber{Decimal: decimal.RequireFromString(offer.PricePerStock)}, Currency: offer.Currency},
		SettlementDate: offer.SettlementDate,
		Amount:         offer.Amount,
	}
```

(Confirm `offer.PricePerStock` is a decimal string here; it is set from the negotiation. `decimal` is already imported in this file.)

- [ ] **Step 2: Update the executor's local `optionDescriptionForCheck` + reads**

In `transaction-service/internal/sitx/posting_executor.go`, change the local struct at `:96-103` to read nested fields and drop `Intent`:

```go
type optionDescriptionForCheck struct {
	Stock  StockDescription `json:"stock"`
	Amount int64            `json:"amount"`
}

type StockDescription struct {
	Ticker string `json:"ticker"`
}
```

In the validation pre-pass (~`:200-211`) the code unmarshals into `contractsitx.OptionDescription` and reads `full.Ticker` / `full.StrikePrice` / `full.Intent`. Replace those reads:
- `full.Ticker` → `full.Stock.Ticker`
- `full.StrikePrice.String()` → `full.PricePerUnit.Amount.Decimal.String()`
- `Intent: full.Intent` → `Intent: contractsitx.OptionIntentAccept` (a new constant; see Step 3). The pre-pass only runs for OPTION-*asset* legs, which are always accept now.

In the reserve block (~`:256-269`): the guard `if od.Intent != "exercise"` is no longer meaningful (OPTION-asset legs are always accept). Replace the whole `if od.Intent != "exercise" { ... }` wrapper so the seller-share reservation ALWAYS runs for a DEBIT OPTION-asset leg, and update `od.Ticker` → `od.Stock.Ticker`:

```go
			if p.Direction == contractsitx.DirectionDebit {
				var od optionDescriptionForCheck
				_ = json.Unmarshal([]byte(p.AssetID), &od)
				if e.holdingChecker == nil {
					return noVote(contractsitx.NoVoteReasonInsufficientAsset, i)
				}
				if od.Stock.Ticker != "" && od.Amount > 0 {
					seller := sellerByDesc[p.AssetID]
					crossbankTxID := peerBankCode + ":" + locallyGeneratedKey
					resp, err := e.holdingChecker.ReserveSellerSharesForNewTx(ctx, &stockpb.ReserveSellerSharesRequest{
						SellerId:      &stockpb.PeerForeignBankId{RoutingNumber: seller.RoutingNumber, Id: seller.ID},
						Ticker:        od.Stock.Ticker,
						Quantity:      od.Amount,
						CrossbankTxId: crossbankTxID,
					})
					if err != nil || resp == nil || !resp.GetOk() {
						return noVote(contractsitx.NoVoteReasonInsufficientAsset, i)
					}
				}
			}
```

- [ ] **Step 3: Add internal-only `OptionIntent*` constants**

In `contract/sitx/otc_types.go` (or `types.go`), add:

```go
// Option intents are INTERNAL ONLY — never serialized to the wire. The
// receiver derives accept vs exercise from transaction shape (OPTION asset
// vs OPTION pseudo-account) and passes the right intent to RecordOptionContract.
const (
	OptionIntentAccept   = "accept"
	OptionIntentExercise = "exercise"
)
```

- [ ] **Step 4: Update the COMMIT-side intent source**

In `transaction-service/internal/handler/peer_tx_grpc_handler.go:430`, the `RecordOptionContract` call currently passes `Intent: od.Intent` (parsed from the option JSON). OPTION-asset OptionItems are always accept now. Replace with:

```go
			Intent: contractsitx.OptionIntentAccept,
```

Remove the now-unused `od` unmarshal at `:422` if `od` is used only for `Intent` (check: if `od` has no other reads, delete the `var od ...; _ = json.Unmarshal(...)` lines).

- [ ] **Step 5: Build the three modules**

Run: `cd contract && go build ./... && cd ../stock-service && go build ./... && cd ../transaction-service && go build ./...`
Expected: all build. (The exercise builder at `peer_otc_grpc_handler.go:1453` still sets old fields — it is rewritten in Task 4; if it breaks the build now, apply Task 4's Step 1 builder change in the same pass so the module compiles, then return here.)

- [ ] **Step 6: Run unit tests for touched modules**

Run: `cd contract && go test ./sitx/... && cd ../transaction-service && go test ./internal/sitx/... ./internal/handler/...`
Expected: PASS (some option tests may need field-name updates — fix any that reference `.Ticker`/`.StrikePrice`/`.Intent` on `OptionDescription` to the nested fields).

- [ ] **Step 7: Commit**

```bash
git add contract/sitx stock-service/internal/handler/peer_otc_grpc_handler.go transaction-service/internal/sitx/posting_executor.go transaction-service/internal/handler/peer_tx_grpc_handler.go
git commit -m "feat(sitx): reshape OptionDescription to spec (nested stock/pricePerUnit, drop intent)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Add the accept-side byte fixture

**Files:**
- Create: `contract/sitx/testdata/newtx_otc_accept.json`
- Modify: `contract/sitx/conformance_test.go`

- [ ] **Step 1: Author the fixture** (`contract/sitx/testdata/newtx_otc_accept.json`)

```json
{
  "idempotenceKey": { "routingNumber": 111, "locallyGeneratedKey": "k-otc-accept-1" },
  "messageType": "NEW_TX",
  "message": {
    "postings": [
      { "account": { "type": "ACCOUNT", "num": "111000117810858011" }, "amount": -1000, "asset": { "type": "MONAS", "asset": { "currency": "RSD" } } },
      { "account": { "type": "PERSON", "id": { "routingNumber": 222, "id": "client-1" } }, "amount": 1000, "asset": { "type": "MONAS", "asset": { "currency": "RSD" } } },
      { "account": { "type": "PERSON", "id": { "routingNumber": 222, "id": "client-1" } }, "amount": -1, "asset": { "type": "OPTION", "asset": { "negotiationId": { "routingNumber": 111, "id": "neg-1" }, "stock": { "ticker": "WMT" }, "pricePerUnit": { "amount": 50, "currency": "RSD" }, "settlementDate": "2026-12-31T00:00:00+02:00", "amount": 10 } } },
      { "account": { "type": "PERSON", "id": { "routingNumber": 111, "id": "client-1" } }, "amount": 1, "asset": { "type": "OPTION", "asset": { "negotiationId": { "routingNumber": 111, "id": "neg-1" }, "stock": { "ticker": "WMT" }, "pricePerUnit": { "amount": 50, "currency": "RSD" }, "settlementDate": "2026-12-31T00:00:00+02:00", "amount": 10 } } }
    ],
    "transactionId": { "routingNumber": 111, "id": "k-otc-accept-1" },
    "message": "Cross-bank OTC otc-accept",
    "paymentCode": "",
    "paymentPurpose": ""
  }
}
```

- [ ] **Step 2: Add the case to `conformance_test.go`**

In the `cases` slice of `TestConformance`, add (build the value with the new `OptionDescription` shape):

```go
		{
			name:    "newtx_otc_accept",
			fixture: "newtx_otc_accept.json",
			value: func() Message[Transaction] {
				od := OptionDescription{
					NegotiationID:  ForeignBankId{RoutingNumber: 111, ID: "neg-1"},
					Stock:          StockDescription{Ticker: "WMT"},
					PricePerUnit:   MonetaryValue{Amount: dn("50"), Currency: "RSD"},
					SettlementDate: "2026-12-31T00:00:00+02:00",
					Amount:         10,
				}
				optAsset := Asset{Type: AssetTypeOption, Asset: od}
				rsd := Asset{Type: AssetTypeMonas, Asset: MonetaryAsset{Currency: "RSD"}}
				return Message[Transaction]{
					IdempotenceKey: IdempotenceKey{RoutingNumber: 111, LocallyGeneratedKey: "k-otc-accept-1"},
					MessageType:    MessageTypeNewTx,
					Message: Transaction{
						Postings: []Posting{
							{Account: TxAccount{Type: AccountTypeAccount, Num: "111000117810858011"}, Amount: dn("-1000"), Asset: rsd},
							{Account: TxAccount{Type: AccountTypePerson, ID: &ForeignBankId{RoutingNumber: 222, ID: "client-1"}}, Amount: dn("1000"), Asset: rsd},
							{Account: TxAccount{Type: AccountTypePerson, ID: &ForeignBankId{RoutingNumber: 222, ID: "client-1"}}, Amount: dn("-1"), Asset: optAsset},
							{Account: TxAccount{Type: AccountTypePerson, ID: &ForeignBankId{RoutingNumber: 111, ID: "client-1"}}, Amount: dn("1"), Asset: optAsset},
						},
						TransactionID:  ForeignBankId{RoutingNumber: 111, ID: "k-otc-accept-1"},
						Message:        "Cross-bank OTC otc-accept",
						PaymentCode:    "",
						PaymentPurpose: "",
					},
				}
			}(),
		},
```

- [ ] **Step 3: Run the conformance test**

Run: `cd contract && go test ./sitx/ -run TestConformance -v`
Expected: PASS (the marshaled value byte-matches the fixture). If it fails, the mismatch printout shows the exact field diff — fix the fixture to match struct field order.

- [ ] **Step 4: Commit**

```bash
git add contract/sitx/testdata/newtx_otc_accept.json contract/sitx/conformance_test.go
git commit -m "test(sitx): byte fixture for accept-side OPTION-asset NEW_TX

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Re-model the exercise builder to the pseudo-account form

**Files:**
- Modify: `stock-service/internal/handler/peer_otc_grpc_handler.go:1448-1481` (`InitiateOptionExercise` postings)
- Test: `stock-service/internal/handler/peer_otc_grpc_handler_test.go`

- [ ] **Step 1: Write the failing builder test**

Add to `stock-service/internal/handler/peer_otc_grpc_handler_test.go` a test that captures the postings passed to a fake `PeerTxService` and asserts the spec shape. Use the existing fake/mocks pattern in that file (check how other tests stub `h.peerTx`). The assertions:

```go
func TestInitiateOptionExercise_SpecPseudoAccountForm(t *testing.T) {
	// Arrange: a peer_option_contract on the BUYER side (buyer routing == ownRouting),
	// WMT, strike 50, qty 10, negotiationId {111,"neg-1"}, currency RSD, status active.
	// Stub h.peerTx to capture the SiTxInitiateWithPostingsRequest.
	// Act: call InitiateOptionExercise with buyer_account_number "111000117810858011".
	// Assert exactly 4 postings:
	//  [0] buyer strike: AccountType ACCOUNT, AccountId "111000117810858011", AssetType MONAS, AssetId "RSD", Amount "500", Direction DEBIT
	//  [1] pseudo strike: AccountType OPTION, AccountId "neg-1", RoutingNumber 111, AssetType MONAS, AssetId "RSD", Amount "500", Direction CREDIT
	//  [2] pseudo stock:  AccountType OPTION, AccountId "neg-1", RoutingNumber 111, AssetType STOCK, AssetId "WMT", Amount "10", Direction DEBIT
	//  [3] buyer stock:   AccountType PERSON, AccountId "client-1", RoutingNumber 111, AssetType STOCK, AssetId "WMT", Amount "10", Direction CREDIT
	// And: NO posting has AssetType OPTION; NO option JSON / intent anywhere.
}
```

Fill in the arrange/stub using the file's existing helpers (mirror an existing `InitiateOptionExercise` or `AcceptNegotiation` test).

- [ ] **Step 2: Run to verify it fails**

Run: `cd stock-service && go test ./internal/handler/ -run TestInitiateOptionExercise_SpecPseudoAccountForm -v`
Expected: FAIL (current builder emits OPTION-asset markers).

- [ ] **Step 3: Rewrite the builder postings**

In `InitiateOptionExercise` (~`:1448-1481`), delete the `optDesc`/`optAssetID` construction (no longer needed) and replace the `postings :=` block with the spec pseudo-account form. `strikeAmount := contract.StrikePrice.Mul(decimal.NewFromInt(contract.Quantity)).String()` stays. Note the OPTION pseudo-account uses the negotiationId as a `PeerForeignBankId`-style account: set `RoutingNumber` to `contract.NegotiationRoutingNumber` and `AccountId` to `contract.NegotiationID`, `AccountType: contractsitx.AccountTypeOption`.

```go
	negRouting := contract.NegotiationRoutingNumber
	negID := contract.NegotiationID
	postings := []*transactionpb.SiTxPosting{
		// 1. Buyer pays strike (MONAS, from the pinned buyer account).
		{RoutingNumber: contract.BuyerRoutingNumber, AccountId: req.GetBuyerAccountNumber(), AccountType: contractsitx.AccountTypeAccount, AssetId: contract.Currency, AssetType: contractsitx.AssetTypeMonas, Amount: strikeAmount, Direction: contractsitx.DirectionDebit},
		// 2. Strike arrives at the option pseudo-account (seller bank credits the seller).
		{RoutingNumber: negRouting, AccountId: negID, AccountType: contractsitx.AccountTypeOption, AssetId: contract.Currency, AssetType: contractsitx.AssetTypeMonas, Amount: strikeAmount, Direction: contractsitx.DirectionCredit},
		// 3. Underlying leaves the option pseudo-account (seller bank releases reserved shares).
		{RoutingNumber: negRouting, AccountId: negID, AccountType: contractsitx.AccountTypeOption, AssetId: contract.Ticker, AssetType: contractsitx.AssetTypeStock, Amount: strconv.FormatInt(contract.Quantity, 10), Direction: contractsitx.DirectionDebit},
		// 4. Underlying arrives at the buyer (buyer bank credits the holding).
		{RoutingNumber: contract.BuyerRoutingNumber, AccountId: contract.BuyerID, AccountType: contractsitx.AccountTypePerson, AssetId: contract.Ticker, AssetType: contractsitx.AssetTypeStock, Amount: strconv.FormatInt(contract.Quantity, 10), Direction: contractsitx.DirectionCredit},
	}
```

Confirm `contract.NegotiationRoutingNumber` and `contract.NegotiationID` exist on the model (they were used in the old `optDesc.NegotiationID`). `strconv` is already imported.

- [ ] **Step 4: Run the builder test**

Run: `cd stock-service && go test ./internal/handler/ -run TestInitiateOptionExercise_SpecPseudoAccountForm -v`
Expected: PASS.

- [ ] **Step 5: Build stock-service**

Run: `cd stock-service && go build ./...`
Expected: builds (the prior live-session `AccountType`/`AssetType` exercise fix is fully replaced by this).

- [ ] **Step 6: Commit**

```bash
git add stock-service/internal/handler/peer_otc_grpc_handler.go stock-service/internal/handler/peer_otc_grpc_handler_test.go
git commit -m "feat(sitx): exercise builder emits spec pseudo-account + STOCK + MONAS form

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Receiver executor — recognize STOCK legs and OPTION pseudo-account legs

This is the deep task. The executor (`transaction-service/internal/sitx/posting_executor.go`) currently handles MONAS legs (account resolution) and OPTION-asset legs (seller share reserve + OptionItem). It must additionally handle, for legs on our routing OR our-contract-owned:
- **STOCK asset on a PERSON/ACCOUNT leg** (buyer's underlying arrival) → at COMMIT, credit the buyer's holding.
- **OPTION pseudo-account legs** (`AccountType == "OPTION"`): the MONAS leg credits the seller's money account; the STOCK leg releases/consumes the seller's reserved shares; both keyed by the contract found via the negotiationId carried as the pseudo-account id. Gate on `settlementDate`/used → `OPTION_USED_OR_EXPIRED`; missing contract → `OPTION_NEGOTIATION_NOT_FOUND`; π·k mismatch → `OPTION_AMOUNT_INCORRECT`.

**Files:**
- Modify: `transaction-service/internal/sitx/posting_executor.go`
- Modify: `transaction-service/internal/handler/peer_tx_grpc_handler.go` (COMMIT-side routing for the new OptionItem kinds)
- Test: `transaction-service/internal/sitx/posting_executor_test.go`

- [ ] **Step 1: Decide leg-ownership rule (read, no code)**

The executor's main loop skips postings where `p.RoutingNumber != e.ownRouting`. OPTION-pseudo-account legs carry `RoutingNumber` = the negotiationId's routing (the negotiation's bank), which is NOT necessarily this bank. So add an exception: an OPTION-pseudo-account leg is "ours" when we hold the referenced contract on the seller side. Introduce a checker dependency on stock-service to answer "do I own the seller side of negotiationId X?" Reuse the existing `SellerHoldingChecker` interface — extend it with a lookup if needed, or add a new minimal interface `PeerOptionContractLookup` with one method:

```go
// In posting_executor.go, near SellerHoldingChecker:
type PeerOptionContractLookup interface {
	// LookupSellerContract returns (ticker, strike, qty, currency, sellerAccountResolvable, settlementDateRFC3339, used, found)
	// for a contract this bank holds on the SELLER side, keyed by negotiationId.
	LookupPeerOptionContract(ctx context.Context, in *stockpb.LookupPeerOptionContractRequest, opts ...grpc.CallOption) (*stockpb.LookupPeerOptionContractResponse, error)
}
```

Add the proto RPC + message to `contract/proto/stock/...` (mirror `ValidatePeerOptionMoneyLeg`) and `make proto`, OR reuse `ValidatePeerOptionMoneyLeg` if its response already carries enough (check its fields first: `grep -n "ValidatePeerOptionMoneyLegResponse" contract/stockpb/stock.pb.go`). Prefer reuse if it returns ok + the stored terms; only add a new RPC if reuse is insufficient.

- [ ] **Step 2: Write failing executor tests**

Add to `transaction-service/internal/sitx/posting_executor_test.go` (use the existing fake `AccountClient` + a fake checker):

```go
// Buyer-side: STOCK arrival on a PERSON leg on our routing → produces a
// stock-credit OptionItem/DebitedItem the commit step will apply; vote YES.
func TestReserve_BuyerStockArrival_VotesYes(t *testing.T) { /* ... */ }

// Seller-side: OPTION pseudo-account MONAS+STOCK legs we own →
// vote YES, surfaces an exercise settlement item.
func TestReserve_OptionPseudoAccount_OwnedContract_VotesYes(t *testing.T) { /* ... */ }

// Expired option → OPTION_USED_OR_EXPIRED.
func TestReserve_OptionPseudoAccount_Expired_VotesNo(t *testing.T) { /* assert reason == NoVoteReasonOptionUsedOrExpired */ }

// Unknown negotiationId → OPTION_NEGOTIATION_NOT_FOUND.
func TestReserve_OptionPseudoAccount_NotFound_VotesNo(t *testing.T) { /* assert reason == NoVoteReasonOptionNegotiationNotFound */ }

// Wrong π·k on the pseudo MONAS leg → OPTION_AMOUNT_INCORRECT.
func TestReserve_OptionPseudoAccount_WrongAmount_VotesNo(t *testing.T) { /* assert reason == NoVoteReasonOptionAmountIncorrect */ }
```

Fill in each with concrete `InternalPosting` slices matching Task 4's wire shape (after `SpecPostingToInternal`). Mirror the existing executor test setup.

- [ ] **Step 3: Run to verify they fail**

Run: `cd transaction-service && go test ./internal/sitx/ -run TestReserve_ -v`
Expected: FAIL.

- [ ] **Step 4: Implement the new recognitions in `Reserve`**

Add helpers and branches:

```go
func isOptionPseudoAccount(p contractsitx.InternalPosting) bool { return p.AccountType == contractsitx.AccountTypeOption }
func isStockLeg(p contractsitx.InternalPosting) bool { return p.AssetType == contractsitx.AssetTypeStock }
```

In the main loop, BEFORE the `if p.RoutingNumber != e.ownRouting { continue }` skip, handle pseudo-account legs with the ownership-by-contract rule: for `isOptionPseudoAccount(p)`, call `LookupPeerOptionContract` (negotiationId = `{p.RoutingNumber, p.AccountID}`). If not found → this leg is not ours, `continue` (the owning bank handles it) — UNLESS our own contract matches, in which case validate and record an exercise-settlement `OptionItem`. Apply the gates:
- contract not found anywhere relevant → only vote NO `OPTION_NEGOTIATION_NOT_FOUND` if no bank could own it (i.e. when WE are the seller bank by registration but have no row). Practical rule: if `LookupPeerOptionContract` returns `found=false`, `continue` (skip) — a different bank owns it; the closed-failure `OPTION_NEGOTIATION_NOT_FOUND` is voted only when the STOCK/MONAS pseudo legs reference a negotiationId we *should* own but can't resolve. Keep this rule explicit and covered by `TestReserve_OptionPseudoAccount_NotFound_VotesNo` (construct that test so the bank is the expected seller bank).
- `used || now > settlementDate` → `noVote(NoVoteReasonOptionUsedOrExpired, i)`.
- pseudo MONAS leg amount != `strike * qty` → `noVote(NoVoteReasonOptionAmountIncorrect, i)`.

For STOCK legs on PERSON/ACCOUNT on our routing (buyer arrival): record an OptionItem of a new kind (carry ticker, qty, owner participant) so COMMIT credits the buyer holding.

Extend `OptionItem` with the fields needed to drive the right COMMIT action (e.g. a `Kind` discriminator: `"accept"`, `"exercise_seller"`, `"exercise_buyer_stock"`), or add a parallel `StockItems []StockCreditItem` to `ReserveResult`. Keep the change minimal and test-driven; the COMMIT step (Task 6) consumes whatever shape you choose. Document the chosen shape in the struct's godoc.

- [ ] **Step 5: Run the executor tests**

Run: `cd transaction-service && go test ./internal/sitx/ -run TestReserve_ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add transaction-service/internal/sitx/posting_executor.go transaction-service/internal/sitx/posting_executor_test.go contract/
git commit -m "feat(sitx): executor recognizes STOCK legs + OPTION pseudo-account exercise legs

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Receiver COMMIT — settle exercise via the existing internal calls

**Files:**
- Modify: `transaction-service/internal/handler/peer_tx_grpc_handler.go` (`materialiseOptions` / the COMMIT option loop ~`:400-440`)
- Test: `transaction-service/internal/handler/peer_tx_grpc_handler_test.go`

- [ ] **Step 1: Write the failing COMMIT test**

Assert that given the exercise `OptionItem`/`StockItem`s from Task 5, the COMMIT step calls the existing settlement RPCs:
- seller side (`exercise_seller`): `RecordOptionContract` with `Intent: contractsitx.OptionIntentExercise`, `Direction: DEBIT`, the negotiationId-derived terms — driving the existing exercise branch (seller money credit + reserved-share consume).
- buyer side (`exercise_buyer_stock`): credits the buyer's holding (the existing `ExerciseBuyerCreditForPeerOption` is invoked via `RecordOptionContract`'s buyer/exercise branch; confirm by reading `peer_otc_grpc_handler.go:1340-1395`).

```go
func TestCommit_Exercise_CallsSettlement(t *testing.T) {
	// fake optionRecorder captures RecordOptionContract calls
	// feed cached exercise OptionItems; call HandleCommitTx
	// assert RecordOptionContract called with Intent=exercise and the right Direction/terms
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd transaction-service && go test ./internal/handler/ -run TestCommit_Exercise_CallsSettlement -v`
Expected: FAIL.

- [ ] **Step 3: Implement COMMIT routing**

In the COMMIT option loop, branch on the OptionItem `Kind`:
- `"accept"` → existing `RecordOptionContract` call with `Intent: contractsitx.OptionIntentAccept` (already done in Task 2 Step 4).
- `"exercise_seller"` → `RecordOptionContract` with `Intent: contractsitx.OptionIntentExercise`, `Direction: DEBIT`, `OptionDescriptionJson` reconstructed from the contract terms (negotiationId/stock/pricePerUnit/settlementDate/amount), buyer/seller ids from the contract.
- `"exercise_buyer_stock"` → `RecordOptionContract` with `Intent: exercise`, `Direction: CREDIT` (buyer side), so the existing buyer-credit branch runs.

Reuse the existing `RecordOptionContract` request construction; only the `Intent`/`Direction`/source-of-terms differ.

- [ ] **Step 4: Run the test**

Run: `cd transaction-service && go test ./internal/handler/ -run TestCommit_Exercise_CallsSettlement -v`
Expected: PASS.

- [ ] **Step 5: Full module build + test + lint**

Run: `cd transaction-service && go build ./... && go test ./... && golangci-lint run ./...`
Then: `cd ../stock-service && go build ./... && go test ./... && golangci-lint run ./...`
Then: `cd ../contract && go test ./...`
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add transaction-service/internal/handler/peer_tx_grpc_handler.go transaction-service/internal/handler/peer_tx_grpc_handler_test.go
git commit -m "feat(sitx): COMMIT routes exercise pseudo-account legs to existing settlement

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: Exercise byte fixture + conformance coverage

**Files:**
- Create: `contract/sitx/testdata/newtx_otc_exercise.json`
- Modify: `contract/sitx/conformance_test.go`

- [ ] **Step 1: Author the fixture** (`contract/sitx/testdata/newtx_otc_exercise.json`)

```json
{
  "idempotenceKey": { "routingNumber": 111, "locallyGeneratedKey": "k-otc-exercise-1" },
  "messageType": "NEW_TX",
  "message": {
    "postings": [
      { "account": { "type": "ACCOUNT", "num": "111000117810858011" }, "amount": -500, "asset": { "type": "MONAS", "asset": { "currency": "RSD" } } },
      { "account": { "type": "OPTION", "id": { "routingNumber": 111, "id": "neg-1" } }, "amount": 500, "asset": { "type": "MONAS", "asset": { "currency": "RSD" } } },
      { "account": { "type": "OPTION", "id": { "routingNumber": 111, "id": "neg-1" } }, "amount": -10, "asset": { "type": "STOCK", "asset": { "ticker": "WMT" } } },
      { "account": { "type": "PERSON", "id": { "routingNumber": 111, "id": "client-1" } }, "amount": 10, "asset": { "type": "STOCK", "asset": { "ticker": "WMT" } } }
    ],
    "transactionId": { "routingNumber": 111, "id": "k-otc-exercise-1" },
    "message": "Cross-bank OTC otc-exercise",
    "paymentCode": "",
    "paymentPurpose": ""
  }
}
```

- [ ] **Step 2: Add the conformance case** (mirror Task 3 Step 2, building the value with `AccountTypeOption` accounts and `AssetTypeStock` assets, amounts `dn("-500")/dn("500")/dn("-10")/dn("10")`).

- [ ] **Step 3: Run conformance**

Run: `cd contract && go test ./sitx/ -run TestConformance -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add contract/sitx/testdata/newtx_otc_exercise.json contract/sitx/conformance_test.go
git commit -m "test(sitx): byte fixture for exercise pseudo-account NEW_TX

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: Two-stack re-verify + Specification.md update

**Files:**
- Modify: `Specification.md` §27 (cross-bank OTC wire types)
- (No code; uses Docker)

- [ ] **Step 1: Bring up both banks** (reuse the session pattern)

Create `.env` (bank A, `OWN_BANK_CODE=111`, `PASSWORD_PEPPER=...`) and ensure `.env.bank-b` (`OWN_BANK_CODE=222`). Then:

```bash
docker compose up -d --build
docker compose --env-file .env.bank-b -f docker-compose.yml -f docker-compose.bank-b.yml up -d --build
```

Wait for both seeders to log "all bootstrapping complete".

- [ ] **Step 2: Run a captured accept→exercise and diff the option legs**

Register peers (A→B via a logging proxy, B→A via a second proxy), create a buyer account at A and a seller account + WMT holding at B, run the negotiation→accept→exercise flow (see the 2026-06-02 session notes in `project_celina5_sitx` memory for the exact curl/python harness). Capture the wire and assert:
- the accept NEW_TX option legs match `newtx_otc_accept.json`'s `OptionDescription` shape (nested `stock`/`pricePerUnit`, **no `intent`**);
- the exercise NEW_TX matches `newtx_otc_exercise.json` (OPTION pseudo-account + STOCK + MONAS, **no OPTION asset, no `intent`**);
- premium + strike money move, the underlying WMT crosses B→A, and the contract reaches `exercised` on both banks.

- [ ] **Step 3: Negative checks**

- Exercise after `settlementDate` (seed a contract with a past `settlementDate`) → vote NO `OPTION_USED_OR_EXPIRED`, ROLLBACK, no money/stock movement.
- Exercise referencing an unknown negotiationId → vote NO `OPTION_NEGOTIATION_NOT_FOUND`.

- [ ] **Step 4: Update `Specification.md` §27**

Replace any flat `{ticker, strikePrice, currency, intent}` option-asset description with the spec `OptionDescription` (`negotiationId, stock, pricePerUnit, settlementDate, amount`), and document the exercise transaction as the OPTION-pseudo-account + STOCK + MONAS form (with the §3.3.1 ownership-by-contract rule and the three `OPTION_*` NoVote reasons). No REST route changes.

- [ ] **Step 5: Tear down + commit docs**

```bash
docker compose down -v
docker compose --env-file .env.bank-b -f docker-compose.yml -f docker-compose.bank-b.yml down -v
git add Specification.md
git commit -m "docs(sitx): Specification §27 — spec OptionDescription + exercise pseudo-account form

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-review notes (for the executor)

- **Hard cutover:** these are breaking wire changes for OTC option flows only. Keep local on `Development`; the cohort flag-day is user-coordinated. Payments/negotiations/discovery are unaffected.
- **Do NOT touch** the internal saga, reservation lifecycle, or `peer_option_contracts` schema. If a task seems to require it, stop — the design is explicitly translation-layer.
- **Money safety gate:** Task 8 Step 2 (money + stock actually move, balances reconcile, contract `exercised`) is the release gate. Do not declare done on green unit tests alone.
- **`mapping.go` is unchanged** — if you find yourself editing it, re-read §3 of the design; the generic OPTION-account/STOCK-asset handling is already there.
