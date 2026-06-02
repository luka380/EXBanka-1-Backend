# SI-TX Wire-Format Conformance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make our cross-bank SI-TX **wire format** match `docs/A protocol for bank-to-bank asset exchange.htm` exactly, while leaving the local saga/executor logic intact.

**Architecture:** Conformance lives in the JSON wire DTOs (`contract/sitx/*`) and the translation functions at the gateway / transaction-service / stock-service boundaries. The internal gRPC proto is *enriched* (additive) so spec data survives translation; the saga consumes the enriched proto unchanged in shape. Hard cutover to the spec dialect; full metadata fidelity.

**Tech Stack:** Go (workspace monorepo), gRPC/protobuf, Gin, GORM/Postgres, `shopspring/decimal`, Kafka. Tests: standard `go test` + `test-app/workflows` integration suite.

**Design reference:** `docs/superpowers/specs/2026-05-31-sitx-wire-conformance-design.md`. Section tags below (e.g. "§6.2") point into it.

## Execution progress (subagent-driven, branch `Development`)

- ✅ **Task 1** — `DecimalNumber` (`2b7897e`, `ed1742a`). Reviewed.
- ✅ **Task 2** — transaction DTOs (`a58abf1`, `ced2c2f`). Reviewed.
- ✅ **Task 3** — `PublicStock[]` bare array (`b8a22fe`). Reviewed. **Note:** this commit also pulled forward **Task 13** (gateway `GetPublicStocks` grouping + otccache `fetchPeer` consume) because the type change broke those consumers. Consume side is test-verified. **Gateway serve side is code-correct but unverifiable until Task 7 restores the api-gateway `handler` package; deepen `TestPeerOTC_GetPublicStocks` to assert inner shape at that point.**
- ✅ **Task 4** — proto enrichment (`943e9c8`). Self-verified (deterministic regen; new symbols confirmed).
- ✅ **Task 5** — sign/direction inversion mapping `contract/sitx/mapping.go` (`a662f11`). 21/21 green. **Reviewed by opus**, which independently re-derived the inversion from spec §2.8 + the real executor (`posting_executor.go:303`): **negative spec amount → internal DEBIT (asset leaves); positive → internal CREDIT (asset arrives)** — confirmed money moves the right way. This is the canonical mapping; all posting translation MUST go through `SpecPostingToInternal`/`InternalPostingToSpec`.
- ✅ **Task 7** — gateway `/interbank` handler → spec envelope (`ebce6e3`, `e845c3d`). **api-gateway fully builds + handler tests green** (incl. deepened `/public-stock` inner-shape assertion = Task 13 serve verified, and a NO-vote full-posting-re-attachment test). Reviewed (approved-with-minor-notes, notes closed).
- ✅ **Task 6** — `IsBalanced` signed-amount balance check (`86e871e`). Done out of original order (was skipped between 5 and 7; no dependency impact).
- **contract module: fully green (Tasks 1–6). api-gateway: fully green (Task 7 + pulled-forward Task 13 serve/consume).**
- ✅ **Tasks 8–12** — transaction-service cluster (`6af34db`, `b62272d`, `92e9952`, `b06a8c8`, `ebea357`) + interop fix `fbea247`. transaction-service + account-service **fully green**. Executor works on `InternalPosting`; NO-vote indices set; tx metadata persisted + surfaced as ledger memo (added `memo` to `CommitIncomingRequest`); **transactionId correlation** with per-message idem (L/M scheme); outbound builds spec signed postings via `InternalPostingToSpec`; sender treats 202 as retry-later.
  - **Deep opus money/saga review:** reservation keys derive from stored NEW_TX key (never the COMMIT idem) → no stranded/double-settled money; signs canonical (DEBIT→negative); 202 cannot trigger rollback. **Important interop fix applied** (`fbea247`): COMMIT/ROLLBACK now correlate via the `tx_foreign_id` column (`LookupByTransactionID`), so a spec-conformant foreign peer that picks `transactionId.id ≠ its NEW_TX idem` is handled (previously would strand holds). Also fixed CHECK_STATUS to key on tx_foreign_id; added rollback-after-commit guard + `(peer, tx_foreign_id)` index.
- **TRANSACTION PROTOCOL (Tasks 1–12) COMPLETE & VERIFIED.** Green modules: contract, api-gateway, transaction-service, account-service.
- ✅ **Task 14** — `/user` → `{bankDisplayName, displayName}` + `OWN_BANK_NAME` config + docker-compose (`989da07`). Reviewed.
- ✅ **Task 15** — OTC money amounts as JSON numbers (`99d16e8`); inbound tolerant of number-or-quoted. Reviewed (no quoted-amount leaks).
- ✅ **Task 16** — stock-service accept postings carry account/asset type tags + `AccountType*`/`AssetType*` constants in `contract/sitx` (`831285b`).
- **ALL IMPLEMENTATION (Tasks 1–16) COMPLETE. Full workspace build+test sweep GREEN** (contract, api-gateway, transaction-service, account-service, stock-service).
- **Repo facts discovered:** no `Specification.md` and no `docker-compose-remote.yml` exist (CLAUDE.md references them but they're absent) → Task 19 = `docs/api/REST_API_v3.md` (already partly updated by Tasks 14/15) + swagger + memory.
- ✅ **Task 17** — byte-level conformance fixtures `contract/sitx/testdata/*.json` + `conformance_test.go` (`2bf5139`). Cohort interop guard.
- ✅ **`UserInformation` struct** aligned to spec `{bankDisplayName, displayName}` (`cfd25ef`, amended to fix a test-build break — lesson: run `go test` not just `go build` before committing).
- ✅ **Task 19** — `docs/api/REST_API_v3.md` documents the full spec wire format (interbank envelope/NEW_TX/vote/commit-rollback, public-stock bare array); swagger regen no-op (`dbecc04`).
- ✅ **Task 18** — integration test `sitx_conformance_test.go` (`//go:build integration`) asserts outbound NEW_TX is spec-shaped + COMMIT correlation; compiles+vets clean (runs under `make docker-up`) (`266f3ce`). Also fixed stale `cohort_dry_run_test.go` mock vote `{"type":"YES"}`→`{"vote":"YES"}` (`25435aa`).
- **🎉 ALL 19 TASKS COMPLETE. Full workspace build+test GREEN (contract, api-gateway, transaction-service, account-service, stock-service); test-app integration compiles under `-tags integration`.**
- **Hard cutover is live: peers must speak the spec dialect. `contract/sitx/testdata/*.json` are the shared byte-level target for the cohort.**

**Known-broken (expected) until their rewrite tasks land:** `api-gateway/internal/handler` (Task 7), `transaction-service/internal/{handler,sitx}` (Tasks 8–12), `stock-service` accept path (Task 16). The contract module + `stock-service/internal/otccache` are green.

**Ground rules for the implementer:**
- TDD: write the failing test, watch it fail, implement, watch it pass, commit.
- Run `make lint` on every service you touch before its commit; zero new warnings.
- Run `make proto` after any `.proto` edit and commit the regenerated files.
- Commit on `Development` (the integration branch). Never push to `main`.
- The **two highest-risk tasks are Task 5 (sign/direction inversion) and Task 11 (transactionId correlation)** — do not batch them; review each alone.

---

## Phase 1 — Wire DTO foundation (pure types; no behavior change)

### Task 1: `DecimalNumber` wire type (§6.1a)

**Files:**
- Create: `contract/sitx/decimalnum.go`
- Test: `contract/sitx/decimalnum_test.go`

- [ ] **Step 1: Write the failing test**

```go
// contract/sitx/decimalnum_test.go
package sitx

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

func TestDecimalNumber_MarshalsAsBareNumber(t *testing.T) {
	d := DecimalNumber{decimal.RequireFromString("260")}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "260" {
		t.Fatalf("want bare number 260, got %s", b)
	}
}

func TestDecimalNumber_MarshalsFraction(t *testing.T) {
	d := DecimalNumber{decimal.RequireFromString("1.5")}
	b, _ := json.Marshal(d)
	if string(b) != "1.5" {
		t.Fatalf("want 1.5, got %s", b)
	}
}

func TestDecimalNumber_UnmarshalNumberAndQuoted(t *testing.T) {
	var a DecimalNumber
	if err := json.Unmarshal([]byte("260"), &a); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if !a.Decimal.Equal(decimal.RequireFromString("260")) {
		t.Fatalf("want 260, got %s", a.Decimal)
	}
	var b DecimalNumber
	if err := json.Unmarshal([]byte(`"1.25"`), &b); err != nil {
		t.Fatalf("unmarshal quoted: %v", err)
	}
	if !b.Decimal.Equal(decimal.RequireFromString("1.25")) {
		t.Fatalf("want 1.25, got %s", b.Decimal)
	}
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `cd contract && go test ./sitx/ -run TestDecimalNumber -v`
Expected: FAIL — `undefined: DecimalNumber`.

- [ ] **Step 3: Implement**

```go
// contract/sitx/decimalnum.go
package sitx

import (
	"strings"

	"github.com/shopspring/decimal"
)

// DecimalNumber wraps decimal.Decimal so it (de)serializes as a JSON
// *number* token rather than a quoted string. SI-TX §2.5 / §2.8.1 require
// monetary amounts to be JSON numbers, while shopspring/decimal defaults to
// quoting. Used only by the wire DTOs; internal storage stays decimal-string.
type DecimalNumber struct {
	decimal.Decimal
}

// MarshalJSON emits the decimal as a bare numeric token (e.g. 260, 1.5).
func (d DecimalNumber) MarshalJSON() ([]byte, error) {
	return []byte(d.Decimal.String()), nil
}

// UnmarshalJSON accepts either a JSON number or a quoted string (tolerant of
// peers that still quote), parsing without float64 rounding.
func (d *DecimalNumber) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	v, err := decimal.NewFromString(s)
	if err != nil {
		return err
	}
	d.Decimal = v
	return nil
}
```

- [ ] **Step 4: Run it, verify it passes**

Run: `cd contract && go test ./sitx/ -run TestDecimalNumber -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add contract/sitx/decimalnum.go contract/sitx/decimalnum_test.go
git commit -m "feat(sitx): add DecimalNumber wire type (JSON number, not string)"
```

---

### Task 2: Rewrite transaction-protocol DTOs (§6.1)

**Files:**
- Modify: `contract/sitx/types.go`
- Test: `contract/sitx/types_wire_test.go` (create)

This replaces the flat `Posting`/`Transaction`/`TransactionVote`/`CommitTransaction`/`RollbackTransaction`/`NoVote` with the spec shapes. `IdempotenceKey`, `Message[T]`, the message-type and reason-code constants, and the vote constants are **unchanged**.

- [ ] **Step 1: Write the failing golden test**

```go
// contract/sitx/types_wire_test.go
package sitx

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

// The §2.8 "coffee" transaction, balanced: 444… credited 260 (negative),
// 111… debited 260 (positive).
func TestTransaction_SpecCoffeeShape(t *testing.T) {
	tx := Transaction{
		Postings: []Posting{
			{
				Account: TxAccount{Type: "ACCOUNT", Num: "444000100182503611"},
				Amount:  DecimalNumber{decimal.RequireFromString("-260")},
				Asset:   Asset{Type: "MONAS", Asset: MonetaryAsset{Currency: "RSD"}},
			},
			{
				Account: TxAccount{Type: "ACCOUNT", Num: "111000141215476411"},
				Amount:  DecimalNumber{decimal.RequireFromString("260")},
				Asset:   Asset{Type: "MONAS", Asset: MonetaryAsset{Currency: "RSD"}},
			},
		},
		TransactionID:  ForeignBankId{RoutingNumber: 111, ID: "tx-1"},
		Message:        "coffee",
		PaymentCode:    "289",
		PaymentPurpose: "debt",
	}
	b, err := json.Marshal(tx)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	want := `{"postings":[{"account":{"type":"ACCOUNT","num":"444000100182503611"},"amount":-260,"asset":{"type":"MONAS","asset":{"currency":"RSD"}}},{"account":{"type":"ACCOUNT","num":"111000141215476411"},"amount":260,"asset":{"type":"MONAS","asset":{"currency":"RSD"}}}],"transactionId":{"routingNumber":111,"id":"tx-1"},"message":"coffee","paymentCode":"289","paymentPurpose":"debt"}`
	if got != want {
		t.Fatalf("wire shape mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestTransactionVote_YesAndNo(t *testing.T) {
	yes, _ := json.Marshal(TransactionVote{Vote: VoteYes})
	if string(yes) != `{"vote":"YES"}` {
		t.Fatalf("yes vote: %s", yes)
	}
	p := Posting{Account: TxAccount{Type: "ACCOUNT", Num: "1"}, Amount: DecimalNumber{decimal.RequireFromString("5")}, Asset: Asset{Type: "MONAS", Asset: MonetaryAsset{Currency: "RSD"}}}
	no, _ := json.Marshal(TransactionVote{Vote: VoteNo, Reasons: []NoVoteReason{{Reason: NoVoteReasonInsufficientAsset, Posting: &p}}})
	want := `{"vote":"NO","reasons":[{"reason":"INSUFFICIENT_ASSET","posting":{"account":{"type":"ACCOUNT","num":"1"},"amount":5,"asset":{"type":"MONAS","asset":{"currency":"RSD"}}}}]}`
	if string(no) != want {
		t.Fatalf("no vote:\n got: %s\nwant: %s", no, want)
	}
}

func TestCommitRollback_ForeignBankId(t *testing.T) {
	c, _ := json.Marshal(CommitTransaction{TransactionID: ForeignBankId{RoutingNumber: 111, ID: "tx-1"}})
	if string(c) != `{"transactionId":{"routingNumber":111,"id":"tx-1"}}` {
		t.Fatalf("commit: %s", c)
	}
}
```

- [ ] **Step 2: Run it, verify it fails to compile**

Run: `cd contract && go test ./sitx/ -run 'TestTransaction_SpecCoffeeShape|TestTransactionVote|TestCommitRollback' -v`
Expected: FAIL — fields like `TxAccount`, `Asset`, `Posting.Account`, `Vote`, `Reasons` undefined.

- [ ] **Step 3: Rewrite `contract/sitx/types.go`**

Replace the `Posting`, `Transaction`, `CommitTransaction`, `RollbackTransaction`, `NoVote`, and `TransactionVote` declarations (keep the package doc, `IdempotenceKey`, `Message[T]`, and all the `const` blocks; the `decimal` import is still used). Add `ForeignBankId` only if not already declared in this package — it lives in `otc_types.go`, so do **not** redeclare it. New declarations:

```go
// TxAccount is the SI-TX tagged union (§2.6). PERSON/OPTION carry a
// ForeignBankId; ACCOUNT carries a bare currency account number.
type TxAccount struct {
	Type string         `json:"type"`          // "PERSON" | "ACCOUNT" | "OPTION"
	ID   *ForeignBankId `json:"id,omitempty"`  // PERSON, OPTION
	Num  string         `json:"num,omitempty"` // ACCOUNT
}

// MonetaryAsset / StockDescription are the §2.7 asset payloads.
type MonetaryAsset struct {
	Currency string `json:"currency"`
}
type StockDescription struct {
	Ticker string `json:"ticker"`
}

// Asset is the §2.7 tagged union. Asset holds MonetaryAsset, StockDescription,
// or OptionDescription depending on Type.
type Asset struct {
	Type  string      `json:"type"`  // "MONAS" | "STOCK" | "OPTION"
	Asset interface{} `json:"asset"` // MonetaryAsset | StockDescription | OptionDescription
}

// Posting is one §2.8.1 double-entry leg. Amount is SIGNED: negative = credit
// (asset leaves the account), positive = debit (asset arrives). No direction
// field. Amount serializes as a JSON number (DecimalNumber).
type Posting struct {
	Account TxAccount     `json:"account"`
	Amount  DecimalNumber `json:"amount"`
	Asset   Asset         `json:"asset"`
}

// Transaction is the body of a NEW_TX message (§2.8.2).
type Transaction struct {
	Postings       []Posting     `json:"postings"`
	TransactionID  ForeignBankId `json:"transactionId"`
	Message        string        `json:"message"`
	CallNumber     string        `json:"callNumber,omitempty"`
	PaymentCode    string        `json:"paymentCode"`
	PaymentPurpose string        `json:"paymentPurpose"`
}

// CommitTransaction / RollbackTransaction (§2.12.2 / §2.12.3) reference the
// initiator's transactionId as a ForeignBankId.
type CommitTransaction struct {
	TransactionID ForeignBankId `json:"transactionId"`
}
type RollbackTransaction struct {
	TransactionID ForeignBankId `json:"transactionId"`
}

// NoVoteReason (§2.12.1). Posting is the FULL offending posting (not an index).
type NoVoteReason struct {
	Reason  string   `json:"reason"`
	Posting *Posting `json:"posting,omitempty"`
}

// TransactionVote is the NEW_TX response (§2.12.1). Vote is "YES" | "NO";
// Reasons is present only on NO.
type TransactionVote struct {
	Vote    string         `json:"vote"`
	Reasons []NoVoteReason `json:"reasons,omitempty"`
}
```

Notes for the implementer:
- The `MonetaryAsset` / `StockDescription` here may already be declared (Task 3 adds `StockDescription` to `otc_types.go`). Declare each **exactly once** in the package — put `StockDescription` and `MonetaryValue`/`MonetaryAsset` in `otc_types.go` (Task 3) and reference them here. If you hit a redeclaration error, remove the duplicate from `types.go`.
- Delete the now-unused old constants? No — keep all reason/vote/message-type/direction constants; the `Direction*` constants are still used by the internal proto mapping (Task 5).

- [ ] **Step 4: Run it, verify it passes**

Run: `cd contract && go test ./sitx/ -v`
Expected: PASS. (Other packages won't compile yet — that's expected; later tasks fix them.)

- [ ] **Step 5: Commit**

```bash
git add contract/sitx/types.go contract/sitx/types_wire_test.go
git commit -m "feat(sitx): spec-shaped transaction DTOs (TxAccount/Asset unions, signed amount, vote)"
```

---

### Task 3: Spec `PublicStock[]` bare array + shared OTC value types (§6.8a)

**Files:**
- Modify: `contract/sitx/otc_types.go`
- Test: `contract/sitx/otc_wire_test.go` (create)

Rewrite `PublicStock` / `PublicStocksResponse` to the spec bare-array shape and add the shared `MonetaryValue` value type. Leave `OtcOffer`, `OtcNegotiation`, `OptionDescription`, `UserInformation`, `PublicOptionOffer*` as-is structurally (they are internal/cohort-extension, not the negotiation HTTP wire — §1.1), but switch any money field that reaches the wire to `DecimalNumber` if and only if a later task shows it is serialized to a peer. For this task, only `PublicStock*` and `MonetaryValue` change.

- [ ] **Step 1: Write the failing test**

```go
// contract/sitx/otc_wire_test.go
package sitx

import (
	"encoding/json"
	"testing"
)

func TestPublicStocksResponse_BareArrayWithSellers(t *testing.T) {
	resp := PublicStocksResponse{
		{
			Stock: StockDescription{Ticker: "AAPL"},
			Sellers: []PublicSeller{
				{Seller: ForeignBankId{RoutingNumber: 111, ID: "client-3"}, Amount: 50},
			},
		},
	}
	b, _ := json.Marshal(resp)
	want := `[{"stock":{"ticker":"AAPL"},"sellers":[{"seller":{"routingNumber":111,"id":"client-3"},"amount":50}]}]`
	if string(b) != want {
		t.Fatalf("public-stock shape:\n got: %s\nwant: %s", b, want)
	}
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `cd contract && go test ./sitx/ -run TestPublicStocksResponse -v`
Expected: FAIL — `PublicSeller` undefined / `PublicStocksResponse` is a struct, not a slice.

- [ ] **Step 3: Edit `contract/sitx/otc_types.go`**

- Ensure `StockDescription` is declared **once** in the package (put the canonical one here; remove it from `types.go` if you added it there).
- Add `MonetaryValue` (spec §2.5 wire shape):

```go
// MonetaryValue is the SI-TX §2.5 money value. Amount is a JSON number.
type MonetaryValue struct {
	Currency string        `json:"currency"`
	Amount   DecimalNumber `json:"amount"`
}
```

- Replace the existing `PublicStock` and `PublicStocksResponse` declarations with:

```go
// PublicSeller is one seller of a public stock (§3.1).
type PublicSeller struct {
	Seller ForeignBankId `json:"seller"`
	Amount int64         `json:"amount"`
}

// PublicStock groups all sellers of one ticker (§3.1).
type PublicStock struct {
	Stock   StockDescription `json:"stock"`
	Sellers []PublicSeller   `json:"sellers"`
}

// PublicStocksResponse is the §3.1 response: a BARE array.
type PublicStocksResponse []PublicStock
```

Remove the old `PublicStock{OwnerID, Ticker, Amount, PricePerStock, Currency}` and `PublicStocksResponse{Stocks []PublicStock}` definitions.

- [ ] **Step 4: Run it, verify it passes**

Run: `cd contract && go test ./sitx/ -run TestPublicStocksResponse -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add contract/sitx/otc_types.go contract/sitx/otc_wire_test.go
git commit -m "feat(sitx): spec-shaped PublicStock bare array + MonetaryValue"
```

---

## Phase 2 — Internal proto enrichment

### Task 4: Enrich the transaction proto (§6.7)

**Files:**
- Modify: `contract/proto/transaction/transaction.proto`
- Regenerate: `contract/transactionpb/*` (via `make proto`)

- [ ] **Step 1: Edit the proto** (additive; do not renumber existing fields)

In `SiTxPosting` add two fields:

```proto
message SiTxPosting {
  int64 routing_number = 1;
  string account_id = 2;
  string asset_id = 3;
  string amount = 4;     // decimal as string (magnitude, non-negative)
  string direction = 5;  // DEBIT | CREDIT (internal effect)
  string account_type = 6; // PERSON | ACCOUNT | OPTION
  string asset_type = 7;   // MONAS | STOCK | OPTION
}
```

Add a ForeignBankId message and enrich `SiTxNewTxRequest`:

```proto
message SiTxForeignBankId {
  int64 routing_number = 1;
  string id = 2;
}

message SiTxNewTxRequest {
  SiTxIdempotenceKey idempotence_key = 1;
  string peer_bank_code = 2;
  repeated SiTxPosting postings = 3;
  SiTxForeignBankId transaction_id = 4;
  string message = 5;
  string payment_code = 6;
  string payment_purpose = 7;
  string call_number = 8;
}
```

Change commit/rollback to carry the initiator's ForeignBankId (keep the old string for one release? No — hard cutover; replace):

```proto
message SiTxCommitRequest {
  SiTxIdempotenceKey idempotence_key = 1;
  string peer_bank_code = 2;
  SiTxForeignBankId transaction_id = 3;
}
message SiTxRollbackRequest {
  SiTxIdempotenceKey idempotence_key = 1;
  string peer_bank_code = 2;
  SiTxForeignBankId transaction_id = 3;
}
```

Leave `SiTxVoteResponse.transaction_id` in the proto (used internally), but later tasks stop emitting it to peers.

- [ ] **Step 2: Regenerate**

Run: `make proto`
Expected: `contract/transactionpb/transaction.pb.go` updated, no errors.

- [ ] **Step 3: Verify it compiles**

Run: `cd contract && go build ./...`
Expected: success (callers in other services will break until later tasks; that's fine — only `contract` must build here).

- [ ] **Step 4: Commit**

```bash
git add contract/proto/transaction/transaction.proto contract/transactionpb/
git commit -m "feat(sitx): enrich transaction proto (account/asset type, tx-id ForeignBankId, tx metadata)"
```

---

## Phase 3 — Mapping helpers (pure functions; HIGH CARE)

### Task 5: Spec ↔ internal posting mapping with sign/direction inversion (§6.2) — HIGHEST RISK

**Files:**
- Create: `contract/sitx/mapping.go` (shared, pure; no service deps)
- Test: `contract/sitx/mapping_test.go`

This pair of functions is the single place the inversion lives. Put it in `contract/sitx` so both gateway and transaction-service use the same code.

- [ ] **Step 1: Write the failing test (assert the inversion + round-trip)**

```go
// contract/sitx/mapping_test.go
package sitx

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestSpecPostingToInternal_NegativeIsDebitOutgoing(t *testing.T) {
	// Spec: negative amount = credit = asset LEAVES = internal DEBIT.
	p := Posting{
		Account: TxAccount{Type: "ACCOUNT", Num: "444000100182503611"},
		Amount:  DecimalNumber{decimal.RequireFromString("-260")},
		Asset:   Asset{Type: "MONAS", Asset: map[string]interface{}{"currency": "RSD"}},
	}
	ip, err := SpecPostingToInternal(p)
	if err != nil {
		t.Fatal(err)
	}
	if ip.Direction != DirectionDebit {
		t.Fatalf("negative amount must map to internal DEBIT, got %s", ip.Direction)
	}
	if ip.Amount != "260" {
		t.Fatalf("magnitude must be abs, got %s", ip.Amount)
	}
	if ip.AccountType != "ACCOUNT" || ip.AccountID != "444000100182503611" {
		t.Fatalf("account mapping wrong: %+v", ip)
	}
	if ip.AssetType != "MONAS" || ip.AssetID != "RSD" {
		t.Fatalf("asset mapping wrong: %+v", ip)
	}
}

func TestSpecPostingToInternal_PositiveIsCreditIncoming(t *testing.T) {
	p := Posting{
		Account: TxAccount{Type: "ACCOUNT", Num: "111000141215476411"},
		Amount:  DecimalNumber{decimal.RequireFromString("260")},
		Asset:   Asset{Type: "MONAS", Asset: map[string]interface{}{"currency": "RSD"}},
	}
	ip, _ := SpecPostingToInternal(p)
	if ip.Direction != DirectionCredit {
		t.Fatalf("positive amount must map to internal CREDIT, got %s", ip.Direction)
	}
}

func TestPostingRoundTrip(t *testing.T) {
	orig := Posting{
		Account: TxAccount{Type: "PERSON", ID: &ForeignBankId{RoutingNumber: 222, ID: "client-7"}},
		Amount:  DecimalNumber{decimal.RequireFromString("-12.5")},
		Asset:   Asset{Type: "MONAS", Asset: map[string]interface{}{"currency": "EUR"}},
	}
	ip, err := SpecPostingToInternal(orig)
	if err != nil {
		t.Fatal(err)
	}
	back, err := InternalPostingToSpec(ip)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Amount.Equal(orig.Amount.Decimal) {
		t.Fatalf("amount round-trip: got %s want %s", back.Amount.Decimal, orig.Amount.Decimal)
	}
	if back.Account.Type != "PERSON" || back.Account.ID.ID != "client-7" {
		t.Fatalf("account round-trip wrong: %+v", back.Account)
	}
	if back.Asset.Type != "MONAS" {
		t.Fatalf("asset round-trip wrong: %+v", back.Asset)
	}
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `cd contract && go test ./sitx/ -run 'TestSpecPosting|TestPostingRoundTrip' -v`
Expected: FAIL — `SpecPostingToInternal`, `InternalPosting`, `InternalPostingToSpec` undefined.

- [ ] **Step 3: Implement `contract/sitx/mapping.go`**

```go
// contract/sitx/mapping.go
package sitx

import (
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"
)

// InternalPosting is the flat, decimal-string posting carried over gRPC to the
// executor. Direction is the INTERNAL effect (DEBIT = asset leaves / outgoing,
// CREDIT = asset arrives / incoming) — the inverse of the spec's bookkeeping
// word. Amount is the non-negative magnitude.
type InternalPosting struct {
	RoutingNumber int64
	AccountType   string // PERSON | ACCOUNT | OPTION
	AccountID     string // num, or ForeignBankId.id, or negotiation id
	AssetType     string // MONAS | STOCK | OPTION
	AssetID       string // currency, ticker, or option-terms JSON
	Direction     string // DirectionDebit | DirectionCredit
	Amount        string // decimal string, magnitude (>= 0)
}

// SpecPostingToInternal maps a spec Posting to the internal representation,
// applying the sign→direction inversion (§6.2): spec negative (credit, asset
// leaves) → internal DEBIT; spec positive (debit, asset arrives) → internal
// CREDIT.
func SpecPostingToInternal(p Posting) (InternalPosting, error) {
	ip := InternalPosting{AccountType: p.Account.Type, AssetType: p.Asset.Type}

	switch p.Account.Type {
	case "ACCOUNT":
		ip.AccountID = p.Account.Num
		ip.RoutingNumber = routingFromAccountNumber(p.Account.Num)
	case "PERSON", "OPTION":
		if p.Account.ID == nil {
			return ip, fmt.Errorf("account type %s requires id", p.Account.Type)
		}
		ip.AccountID = p.Account.ID.ID
		ip.RoutingNumber = p.Account.ID.RoutingNumber
	default:
		return ip, fmt.Errorf("unknown account type %q", p.Account.Type)
	}

	assetID, err := assetToID(p.Asset)
	if err != nil {
		return ip, err
	}
	ip.AssetID = assetID

	amt := p.Amount.Decimal
	if amt.IsNegative() {
		ip.Direction = DirectionDebit // asset leaves
	} else {
		ip.Direction = DirectionCredit // asset arrives
	}
	ip.Amount = amt.Abs().String()
	return ip, nil
}

// InternalPostingToSpec is the inverse used on the outbound path.
func InternalPostingToSpec(ip InternalPosting) (Posting, error) {
	var acc TxAccount
	switch ip.AccountType {
	case "ACCOUNT":
		acc = TxAccount{Type: "ACCOUNT", Num: ip.AccountID}
	case "PERSON", "OPTION":
		acc = TxAccount{Type: ip.AccountType, ID: &ForeignBankId{RoutingNumber: ip.RoutingNumber, ID: ip.AccountID}}
	default:
		return Posting{}, fmt.Errorf("unknown account type %q", ip.AccountType)
	}

	asset, err := idToAsset(ip.AssetType, ip.AssetID)
	if err != nil {
		return Posting{}, err
	}

	mag, err := decimal.NewFromString(ip.Amount)
	if err != nil {
		return Posting{}, err
	}
	signed := mag.Abs()
	if ip.Direction == DirectionDebit {
		signed = signed.Neg() // internal DEBIT → spec negative (credit)
	}
	return Posting{Account: acc, Amount: DecimalNumber{signed}, Asset: asset}, nil
}

// assetToID extracts the internal asset id string from a spec Asset.
func assetToID(a Asset) (string, error) {
	switch a.Type {
	case "MONAS":
		return fieldString(a.Asset, "currency")
	case "STOCK":
		return fieldString(a.Asset, "ticker")
	case "OPTION":
		// Internal representation keeps the option terms as JSON in assetId.
		b, err := json.Marshal(a.Asset)
		if err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("unknown asset type %q", a.Type)
	}
}

// idToAsset rebuilds a spec Asset from internal type+id.
func idToAsset(assetType, assetID string) (Asset, error) {
	switch assetType {
	case "MONAS":
		return Asset{Type: "MONAS", Asset: MonetaryAsset{Currency: assetID}}, nil
	case "STOCK":
		return Asset{Type: "STOCK", Asset: StockDescription{Ticker: assetID}}, nil
	case "OPTION":
		var od OptionDescription
		if err := json.Unmarshal([]byte(assetID), &od); err != nil {
			return Asset{}, err
		}
		return Asset{Type: "OPTION", Asset: od}, nil
	default:
		return Asset{}, fmt.Errorf("unknown asset type %q", assetType)
	}
}

// fieldString reads a string field from either a typed struct (marshalled) or a
// map[string]interface{} (as produced by json.Unmarshal into Asset.Asset).
func fieldString(v interface{}, key string) (string, error) {
	switch m := v.(type) {
	case map[string]interface{}:
		s, _ := m[key].(string)
		return s, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		var mm map[string]interface{}
		if err := json.Unmarshal(b, &mm); err != nil {
			return "", err
		}
		s, _ := mm[key].(string)
		return s, nil
	}
}

// routingFromAccountNumber reads the 3-digit routing prefix of an 18-digit
// account number; returns 0 if too short.
func routingFromAccountNumber(num string) int64 {
	if len(num) < 3 {
		return 0
	}
	var r int64
	for _, c := range num[:3] {
		if c < '0' || c > '9' {
			return 0
		}
		r = r*10 + int64(c-'0')
	}
	return r
}
```

- [ ] **Step 4: Run it, verify it passes**

Run: `cd contract && go test ./sitx/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add contract/sitx/mapping.go contract/sitx/mapping_test.go
git commit -m "feat(sitx): spec<->internal posting mapping with sign/direction inversion"
```

---

### Task 6: Balance check on signed amounts (§6.3, `UNBALANCED_TX`)

**Files:**
- Create: `contract/sitx/balance.go`
- Test: `contract/sitx/balance_test.go`

- [ ] **Step 1: Write the failing test**

```go
// contract/sitx/balance_test.go
package sitx

import (
	"testing"

	"github.com/shopspring/decimal"
)

func monas(num, amt, ccy string) Posting {
	return Posting{
		Account: TxAccount{Type: "ACCOUNT", Num: num},
		Amount:  DecimalNumber{decimal.RequireFromString(amt)},
		Asset:   Asset{Type: "MONAS", Asset: MonetaryAsset{Currency: ccy}},
	}
}

func TestIsBalanced_TrueWhenSumZeroPerAsset(t *testing.T) {
	tx := []Posting{monas("444", "-260", "RSD"), monas("111", "260", "RSD")}
	if !IsBalanced(tx) {
		t.Fatal("want balanced")
	}
}

func TestIsBalanced_FalseWhenAssetSumNonZero(t *testing.T) {
	tx := []Posting{monas("444", "-260", "RSD"), monas("111", "100", "RSD")}
	if IsBalanced(tx) {
		t.Fatal("want unbalanced")
	}
}

func TestIsBalanced_PerAssetIndependent(t *testing.T) {
	// EUR balances, RSD does not.
	tx := []Posting{monas("1", "-5", "EUR"), monas("2", "5", "EUR"), monas("3", "-9", "RSD")}
	if IsBalanced(tx) {
		t.Fatal("want unbalanced (RSD off)")
	}
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `cd contract && go test ./sitx/ -run TestIsBalanced -v`
Expected: FAIL — `IsBalanced` undefined.

- [ ] **Step 3: Implement**

```go
// contract/sitx/balance.go
package sitx

import "github.com/shopspring/decimal"

// IsBalanced reports whether, for every asset, the signed amounts of all
// postings sum to zero (§2.8). Assets are keyed by type+id so MONAS:RSD and
// STOCK:AAPL are checked independently.
func IsBalanced(postings []Posting) bool {
	sums := map[string]decimal.Decimal{}
	for _, p := range postings {
		key, err := assetKey(p.Asset)
		if err != nil {
			return false
		}
		sums[key] = sums[key].Add(p.Amount.Decimal)
	}
	for _, s := range sums {
		if !s.IsZero() {
			return false
		}
	}
	return true
}

func assetKey(a Asset) (string, error) {
	id, err := assetToID(a)
	if err != nil {
		return "", err
	}
	return a.Type + ":" + id, nil
}
```

- [ ] **Step 4: Run it, verify it passes**

Run: `cd contract && go test ./sitx/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add contract/sitx/balance.go contract/sitx/balance_test.go
git commit -m "feat(sitx): balanced-transaction check on signed amounts"
```

---

## Phase 4 — Gateway translation rewrite

### Task 7: Rewrite `peer_tx_handler.go` to the spec envelope (§6.1/§6.2/§6.4)

**Files:**
- Modify: `api-gateway/internal/handler/peer_tx_handler.go`
- Test: `api-gateway/internal/handler/peer_tx_handler_test.go` (update existing)

The handler must: decode the spec `Message[Transaction]`, reject unbalanced TXs early (optional — the service also checks), translate each spec posting via `SpecPostingToInternal`, pass the enriched fields + metadata + `transactionId` to `HandleNewTx`, and render the spec `TransactionVote`. For COMMIT/ROLLBACK, decode `Message[CommitTransaction]` / `Message[RollbackTransaction]` and pass `transaction_id` as `SiTxForeignBankId`.

- [ ] **Step 1: Update the test** to assert spec wire I/O. Read the existing `peer_tx_handler_test.go` first; replace the NEW_TX request body with a spec-shaped one and assert a spec-shaped vote. Example new test body:

```go
func TestPostInterbank_NewTx_SpecShape(t *testing.T) {
	// Arrange a fake PeerTxServiceClient that records the request and returns YES.
	fake := &fakePeerTxClient{voteType: "YES"}
	h := NewPeerTxHandler(fake)

	body := `{"idempotenceKey":{"routingNumber":222,"locallyGeneratedKey":"k1"},"messageType":"NEW_TX","message":{"postings":[{"account":{"type":"ACCOUNT","num":"444000100182503611"},"amount":-260,"asset":{"type":"MONAS","asset":{"currency":"RSD"}}},{"account":{"type":"ACCOUNT","num":"111000141215476411"},"amount":260,"asset":{"type":"MONAS","asset":{"currency":"RSD"}}}],"transactionId":{"routingNumber":222,"id":"k1"},"message":"coffee","paymentCode":"289","paymentPurpose":"debt"}}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("peer_bank_code", "222")
	c.Request = httptest.NewRequest(http.MethodPost, "/interbank", strings.NewReader(body))

	h.PostInterbank(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"vote":"YES"}` {
		t.Fatalf("vote body: %s", w.Body.String())
	}
	// Assert the translated request carried the enriched fields:
	if len(fake.lastNewTx.GetPostings()) != 2 {
		t.Fatalf("postings not forwarded")
	}
	if fake.lastNewTx.GetPostings()[0].GetDirection() != "DEBIT" { // -260 → DEBIT
		t.Fatalf("inversion wrong: %s", fake.lastNewTx.GetPostings()[0].GetDirection())
	}
	if fake.lastNewTx.GetTransactionId().GetId() != "k1" || fake.lastNewTx.GetMessage() != "coffee" {
		t.Fatalf("metadata/tx-id not forwarded: %+v", fake.lastNewTx)
	}
}
```

(Define/extend `fakePeerTxClient` to implement `transactionpb.PeerTxServiceClient`, capturing `lastNewTx` and returning `&transactionpb.SiTxVoteResponse{Type:"YES"}`. If a fake already exists in the test file, extend it.)

- [ ] **Step 2: Run it, verify it fails**

Run: `cd api-gateway && go test ./internal/handler/ -run TestPostInterbank_NewTx_SpecShape -v`
Expected: FAIL (handler still builds the old flat request / compile error on removed types).

- [ ] **Step 3: Rewrite the handler**

Replace `postingsToProto`, `voteToJSON`, and the three `case` blocks. Key parts:

```go
import (
	// ...
	"github.com/exbanka/contract/sitx"
	transactionpb "github.com/exbanka/contract/transactionpb"
)

func specPostingsToProto(ps []sitx.Posting) ([]*transactionpb.SiTxPosting, error) {
	out := make([]*transactionpb.SiTxPosting, len(ps))
	for i, p := range ps {
		ip, err := sitx.SpecPostingToInternal(p)
		if err != nil {
			return nil, err
		}
		out[i] = &transactionpb.SiTxPosting{
			RoutingNumber: ip.RoutingNumber,
			AccountId:     ip.AccountID,
			AssetId:       ip.AssetID,
			Amount:        ip.Amount,
			Direction:     ip.Direction,
			AccountType:   ip.AccountType,
			AssetType:     ip.AssetType,
		}
	}
	return out, nil
}

func fbIDToProto(f sitx.ForeignBankId) *transactionpb.SiTxForeignBankId {
	return &transactionpb.SiTxForeignBankId{RoutingNumber: f.RoutingNumber, Id: f.ID}
}

func specVoteToJSON(v *transactionpb.SiTxVoteResponse) sitx.TransactionVote {
	out := sitx.TransactionVote{Vote: v.GetType()}
	for _, nv := range v.GetNoVotes() {
		r := sitx.NoVoteReason{Reason: nv.GetReason()}
		// The service returns the offending posting index; the gateway already
		// has the decoded spec postings, so re-attach the full posting.
		if nv.GetPostingIndexSet() {
			// caller passes the decoded postings; see PostInterbank below.
		}
		out.Reasons = append(out.Reasons, r)
	}
	return out
}
```

In `PostInterbank`'s `NEW_TX` case:

```go
case sitx.MessageTypeNewTx:
	var msg sitx.Message[sitx.Transaction]
	if err := json.Unmarshal(body, &msg); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	postings, err := specPostingsToProto(msg.Message.Postings)
	if err != nil {
		apiError(c, http.StatusBadRequest, ErrValidation, err.Error())
		return
	}
	req := &transactionpb.SiTxNewTxRequest{
		IdempotenceKey: idemToProto(msg.IdempotenceKey),
		PeerBankCode:   pbCode,
		Postings:       postings,
		TransactionId:  fbIDToProto(msg.Message.TransactionID),
		Message:        msg.Message.Message,
		PaymentCode:    msg.Message.PaymentCode,
		PaymentPurpose: msg.Message.PaymentPurpose,
		CallNumber:     msg.Message.CallNumber,
	}
	resp, err := h.client.HandleNewTx(c.Request.Context(), req)
	if err != nil {
		renderPeerGRPCError(c, err)
		return
	}
	// Build spec vote, re-attaching the full offending posting by index.
	vote := sitx.TransactionVote{Vote: resp.GetType()}
	for _, nv := range resp.GetNoVotes() {
		r := sitx.NoVoteReason{Reason: nv.GetReason()}
		if nv.GetPostingIndexSet() {
			idx := int(nv.GetPostingIndex())
			if idx >= 0 && idx < len(msg.Message.Postings) {
				p := msg.Message.Postings[idx]
				r.Posting = &p
			}
		}
		vote.Reasons = append(vote.Reasons, r)
	}
	c.JSON(http.StatusOK, vote)
```

COMMIT/ROLLBACK cases decode `sitx.Message[sitx.CommitTransaction]` / `...RollbackTransaction` and set `TransactionId: fbIDToProto(msg.Message.TransactionID)` on `SiTxCommitRequest`/`SiTxRollbackRequest`. Remove the now-unused `postingsToProto`/`voteToJSON`.

- [ ] **Step 4: Run it, verify it passes**

Run: `cd api-gateway && go test ./internal/handler/ -run TestPostInterbank -v`
Expected: PASS.

- [ ] **Step 5: Lint + commit**

```bash
cd api-gateway && golangci-lint run ./... && cd ..
git add api-gateway/internal/handler/peer_tx_handler.go api-gateway/internal/handler/peer_tx_handler_test.go
git commit -m "feat(sitx): gateway interbank handler speaks spec envelope (unions, signed amount, tx-id, metadata)"
```

---

## Phase 5 — transaction-service consumption (saga-adjacent)

### Task 8: Executor consumes enriched proto (§6.2)

**Files:**
- Modify: `transaction-service/internal/sitx/posting_executor.go`
- Modify: `transaction-service/internal/handler/peer_tx_grpc_handler.go` (`protoToPostings`)
- Test: `transaction-service/internal/sitx/posting_executor_test.go` (update)

The executor already branches on `p.Direction` (DEBIT→ReserveOutgoing, CREDIT→ReserveIncoming) at `posting_executor.go:303`. With Task 7 forwarding the inverted direction, the **economic behavior is now correct as-is**. The change here is to (a) use `account_type`/`asset_type` instead of string-sniffing (`strings.HasPrefix(assetId,"{")`, `client-` prefix) where present, and (b) keep currency/option handling working.

- [ ] **Step 1: Update a focused test** in `posting_executor_test.go`: feed a posting with `AssetType:"OPTION"` and assert it takes the option path without relying on `{`-prefix sniffing; feed `AccountType:"PERSON"`, `AccountId:"client-5"` and assert participant resolution still runs.

- [ ] **Step 2: Run it, verify it fails** (the executor ignores the new fields).

Run: `cd transaction-service && go test ./internal/sitx/ -run TestExecutor -v`

- [ ] **Step 3: Implement** — in `protoToPostings` (handler) carry `AccountType`/`AssetType` into the internal `contractsitx.InternalPosting` (or extend the local posting struct the executor uses). In `posting_executor.go`, prefer `p.AssetType == "OPTION"` over `strings.HasPrefix(p.AssetID, "{")`, and `p.AccountType == "PERSON"` to trigger `resolveAccountForPosting`. Leave the reserve/credit logic and the `Direction` switch unchanged. (Read the current `Reserve` method first; make minimal edits to the type-detection points only.)

- [ ] **Step 4: Run it, verify it passes.**

Run: `cd transaction-service && go test ./internal/sitx/ -v`

- [ ] **Step 5: Lint + commit**

```bash
cd transaction-service && golangci-lint run ./... && cd ..
git add transaction-service/internal/sitx/posting_executor.go transaction-service/internal/handler/peer_tx_grpc_handler.go transaction-service/internal/sitx/posting_executor_test.go
git commit -m "feat(sitx): executor uses explicit account/asset type tags from enriched proto"
```

---

### Task 9: NO-vote posting index plumbing (§6.3)

**Files:**
- Modify: `transaction-service/internal/sitx/posting_executor.go` (ensure `SiTxNoVote.posting_index` is set for posting-scoped reasons)
- Test: `transaction-service/internal/sitx/posting_executor_test.go`

The gateway (Task 7) re-attaches the full posting from the index. Ensure the executor sets `PostingIndex`/`PostingIndexSet` for every posting-scoped reason (`NO_SUCH_ACCOUNT`, `NO_SUCH_ASSET`, `UNACCEPTABLE_ASSET`, `INSUFFICIENT_ASSET`, `OPTION_*`) and leaves it unset for `UNBALANCED_TX`.

- [ ] **Step 1: Test** — feed a transaction whose 2nd posting credits a non-existent account; assert the vote is NO with `PostingIndex==1`, `PostingIndexSet==true`, reason `NO_SUCH_ACCOUNT`.
- [ ] **Step 2: Run, verify fail** (if index not currently set).
- [ ] **Step 3: Implement** the index-setting in the relevant `noVote(...)` call sites (the helper exists per `BuildPrelimVote`/`Reserve`). Read the current `noVote` signature first.
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Lint + commit**

```bash
cd transaction-service && golangci-lint run ./... && cd ..
git add transaction-service/internal/sitx/posting_executor.go transaction-service/internal/sitx/posting_executor_test.go
git commit -m "feat(sitx): set posting index on posting-scoped NO-vote reasons"
```

---

### Task 10: Metadata columns + ledger surfacing (§6.6)

**Files:**
- Modify: `transaction-service/internal/model/peer_idempotence_record.go` (add `Message`, `PaymentCode`, `PaymentPurpose`, `CallNumber`, `TxRoutingNumber`, `TxForeignID` columns)
- Modify: `transaction-service/internal/model/outbound_peer_tx.go` (same metadata columns)
- Modify: `transaction-service/internal/handler/peer_tx_grpc_handler.go` (`HandleNewTx` persist metadata; `HandleCommitTx` pass `message` as the ledger memo on credits)
- Test: `transaction-service/internal/handler/peer_tx_grpc_handler_test.go`

- [ ] **Step 1: Test** — `HandleNewTx` with `Message:"coffee", PaymentCode:"289"` persists those on the idempotence record; on `HandleCommitTx`, the `CommitIncoming`/credit call carries `memo == "coffee"`. Use the existing fake account client to capture the memo.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** — add the columns (GORM `AutoMigrate` picks them up). In `HandleNewTx`, store the metadata from `req` on the record. In `HandleCommitTx`, thread `rec.Message` into the account-service credit call's `memo`/`description` field (the `CommitIncomingRequest` or the underlying `UpdateBalance` memo — check `account.proto`; add a `memo` field to `CommitIncomingRequest` if absent and regenerate).
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Lint + commit**

```bash
cd transaction-service && golangci-lint run ./... && cd ..
git add transaction-service/internal/model/peer_idempotence_record.go transaction-service/internal/model/outbound_peer_tx.go transaction-service/internal/handler/peer_tx_grpc_handler.go transaction-service/internal/handler/peer_tx_grpc_handler_test.go contract/proto/account/ contract/accountpb/
git commit -m "feat(sitx): persist tx metadata and surface message in ledger description"
```

---

### Task 11: transactionId correlation + per-message idempotence keys (§6.5) — HIGH RISK

**Files:**
- Modify: `transaction-service/internal/handler/peer_tx_grpc_handler.go` (`HandleCommitTx`, `HandleRollbackTx` correlate by `transaction_id`; `InitiateOutboundTx*` assign `transactionId.id = L` and use a fresh idem per message)
- Modify: `transaction-service/internal/sitx/peer_http_client.go` (commit/rollback envelopes carry `transactionId` ForeignBankId + their own idem)
- Test: `transaction-service/internal/handler/peer_tx_grpc_handler_test.go`

- [ ] **Step 1: Test** — receiver: `HandleNewTx` with `transactionId={222,"k1"}` stores it; a later `HandleCommitTx` with `transactionId={222,"k1"}` **but a different idempotenceKey** (`"commit-1"`) resolves the same record and commits. A duplicate `HandleCommitTx` with idem `"commit-1"` is a no-op (dedup). Initiator: `InitiateOutboundTx` sets `Transaction.TransactionID.ID == L` (the NEW_TX idem) and the COMMIT envelope uses a distinct idem.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement.**
  - Receiver: in `HandleNewTx`, persist `TxRoutingNumber`/`TxForeignID` from `req.GetTransactionId()` (already added in Task 10). In `HandleCommitTx`/`HandleRollbackTx`, look up the record by `(peerBankCode, transaction_id.id)` instead of by the message idem; still record the commit/rollback message's own idem for dedup (a small `seen idem` check — reuse the idempotence repo with the message's own key, or a `committed_at` flag on the record).
  - Initiator: in `InitiateOutboundTx`/`InitiateOutboundTxWithPostings`, set `Transaction.TransactionID = sitx.ForeignBankId{RoutingNumber: h.ownRouting, ID: idem}`. Generate a **fresh** idem (`uuid.NewString()`) for the COMMIT and ROLLBACK envelopes; set their `Message.TransactionID = {h.ownRouting, idem}`.
- [ ] **Step 4: Run, verify pass.** Also run the full transaction-service suite to catch saga regressions: `cd transaction-service && go test ./...`
- [ ] **Step 5: Lint + commit**

```bash
cd transaction-service && golangci-lint run ./... && cd ..
git add transaction-service/internal/handler/peer_tx_grpc_handler.go transaction-service/internal/sitx/peer_http_client.go transaction-service/internal/handler/peer_tx_grpc_handler_test.go
git commit -m "feat(sitx): correlate COMMIT/ROLLBACK by transactionId; unique idempotence key per message"
```

---

## Phase 6 — Outbound build + HTTP semantics

### Task 12: Outbound posting build + 202 handling (§6.1/§6.2/§6.9)

**Files:**
- Modify: `transaction-service/internal/handler/peer_tx_grpc_handler.go` (`InitiateOutboundTx*` build spec `sitx.Posting` via `InternalPostingToSpec`; set `Transaction` metadata fields)
- Modify: `transaction-service/internal/sitx/peer_http_client.go` (`postEnvelope`/`PostNewTx`: treat 202 as retry-later, 200 as final, 204 as final-empty)
- Test: `peer_http_client_more_test.go`, `peer_tx_grpc_handler_*_test.go`

- [ ] **Step 1: Test** —
  - Outbound: `InitiateOutboundTx` produces an envelope whose JSON has `postings[0].account.num`, `amount` as a signed number, `asset.type=="MONAS"`, and a populated `transactionId`. (Marshal the envelope the handler builds and assert the shape.)
  - 202: a fake HTTP server returning `202` makes `PostNewTx` return a sentinel "retry later" (not an error that aborts the saga); `200` with a vote body parses the vote; `204` returns no body without error.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement.**
  - Replace the hand-built flat `contractsitx.Posting{...Direction...}` construction in `InitiateOutboundTx`/`InitiateOutboundTxWithPostings` with: build `InternalPosting` values, then `sitx.InternalPostingToSpec(...)` to get spec `Posting`s; populate `Transaction{Postings, TransactionID, Message, PaymentCode, PaymentPurpose}`.
  - In `peer_http_client.go`, read the current `postEnvelope` return handling; add `case http.StatusAccepted: return ErrRetryLater` (define `var ErrRetryLater = errors.New("peer accepted; retry later")`), keep `200` parsing the body, add `204` as final-empty. Ensure the outbound saga treats `ErrRetryLater` as "leave pending, retry on the replay cron" rather than NO/rollback.
- [ ] **Step 4: Run, verify pass.** Run full suite: `cd transaction-service && go test ./...`
- [ ] **Step 5: Lint + commit**

```bash
cd transaction-service && golangci-lint run ./... && cd ..
git add transaction-service/internal/handler/peer_tx_grpc_handler.go transaction-service/internal/sitx/peer_http_client.go transaction-service/internal/sitx/peer_http_client_more_test.go transaction-service/internal/handler/peer_tx_grpc_handler_more_test.go
git commit -m "feat(sitx): outbound builds spec postings/metadata; sender handles 202 retry-later"
```

---

## Phase 7 — OTC conformance

### Task 13: `/public-stock` bare array + sellers grouping (§6.8a)

**Files:**
- Modify: `api-gateway/internal/handler/peer_otc_handler.go` (`GetPublicStocks`)
- Modify: `stock-service/internal/otccache/cache.go` (`fetchPeer` consume DTO)
- Test: `api-gateway/internal/handler/peer_otc_handler_test.go`, `stock-service/internal/otccache/*_test.go`

- [ ] **Step 1: Test (serve)** — `GetPublicStocks` with a fake stock client returning two rows for `AAPL` (owners `client-3`/50, `client-9`/20) and one for `MSFT` produces the bare array `[{stock:{ticker:"AAPL"},sellers:[{seller:{...client-3},amount:50},{seller:{...client-9},amount:20}]},{stock:{ticker:"MSFT"},sellers:[...]}]` — no `{stocks:…}` wrapper.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement (serve)** — replace the `GetPublicStocks` body:

```go
func (h *PeerOTCHandler) GetPublicStocks(c *gin.Context) {
	pbCode, _ := c.Get("peer_bank_code")
	resp, err := h.client.GetPublicStocks(c.Request.Context(), &stockpb.GetPublicStocksRequest{
		PeerBankCode: peerCtxString(pbCode),
	})
	if err != nil {
		handleGRPCError(c, err)
		return
	}
	type seller struct {
		Seller gin.H `json:"seller"`
		Amount int64 `json:"amount"`
	}
	order := []string{}
	byTicker := map[string][]seller{}
	for _, s := range resp.GetStocks() {
		t := s.GetTicker()
		if _, ok := byTicker[t]; !ok {
			order = append(order, t)
		}
		byTicker[t] = append(byTicker[t], seller{
			Seller: gin.H{"routingNumber": s.GetOwnerId().GetRoutingNumber(), "id": s.GetOwnerId().GetId()},
			Amount: s.GetAmount(),
		})
	}
	out := make([]gin.H, 0, len(order))
	for _, t := range order {
		out = append(out, gin.H{"stock": gin.H{"ticker": t}, "sellers": byTicker[t]})
	}
	c.JSON(http.StatusOK, out) // BARE array
}
```

- [ ] **Step 4: Test + implement (consume)** — in `otccache/cache.go` `fetchPeer`, change `var resp sitx.PublicStocksResponse` decoding to iterate the bare array and, for each `(stock, seller)`, append an `Offer{Kind:"remote", BankCode: peerCode, OwnerID: seller.Seller.ID-without-prefix-or-as-is, Ticker: stock.Stock.Ticker, Quantity: seller.Amount}`. (`PublicStocksResponse` is now a slice, so the existing `resp.Stocks` field access must be replaced with ranging over `resp` directly.) Add/adjust the otccache test to feed a spec bare-array JSON body and assert the resulting `Offer` rows.
- [ ] **Step 5: Lint + commit**

```bash
cd api-gateway && golangci-lint run ./... && cd ..
cd stock-service && golangci-lint run ./... && cd ..
git add api-gateway/internal/handler/peer_otc_handler.go api-gateway/internal/handler/peer_otc_handler_test.go stock-service/internal/otccache/
git commit -m "feat(sitx): /public-stock serves+consumes spec bare array grouped by stock"
```

---

### Task 14: `/user` display-name shape + config (§6.8b)

**Files:**
- Modify: `api-gateway/internal/handler/peer_user_handler.go`
- Modify: `api-gateway/internal/config/config.go` (add `OwnBankName`)
- Modify: `api-gateway/cmd/main.go` (wire `OwnBankName` into `NewPeerUserHandler`)
- Modify: `docker-compose.yml`, `docker-compose-remote.yml` (add `OWN_BANK_NAME` to api-gateway env)
- Test: `api-gateway/internal/handler/peer_user_handler_test.go`

- [ ] **Step 1: Test** — `GetUser` for `client-5` (fake client returns first="Ana", last="Ić") returns `{"bankDisplayName":"EXBanka","displayName":"Ana Ić"}` and HTTP 200; unknown rid → 404.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** — add `ownBankDisplayName string` to `PeerUserHandler` + constructor param; replace both success `c.JSON` blocks with the spec shape (`displayName = first + " " + last`). Add `OwnBankName` to config (env `OWN_BANK_NAME`, default = `strconv.FormatInt(ownRouting,10)` if empty) and pass it through `main.go`. Add the env var to both compose files.
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Lint + commit**

```bash
cd api-gateway && golangci-lint run ./... && cd ..
git add api-gateway/internal/handler/peer_user_handler.go api-gateway/internal/handler/peer_user_handler_test.go api-gateway/internal/config/config.go api-gateway/cmd/main.go docker-compose.yml docker-compose-remote.yml
git commit -m "feat(sitx): /user returns {bankDisplayName, displayName} per spec §3.7"
```

---

### Task 15: OTC money as JSON numbers (§6.8c)

**Files:**
- Modify: `api-gateway/internal/handler/peer_otc_handler.go` (`protoOfferToJSON`, `peerMonetaryValueReq`)
- Modify: `api-gateway/internal/handler/peer_otc_initiate_handler.go` (outbound offer maps)
- Test: `api-gateway/internal/handler/peer_otc_handler_test.go`

- [ ] **Step 1: Test** — `GetNegotiation` body has `"pricePerUnit":{"amount":150.5,...}` (number, unquoted) and `"premium":{"amount":2.75,...}`; an inbound `CreateNegotiation` body with numeric `amount` parses correctly; a body with a quoted `amount` also parses (tolerant).
- [ ] **Step 2: Run, verify fail** (amounts currently render as strings).
- [ ] **Step 3: Implement** —
  - `peerMonetaryValueReq.Amount`: change to `sitx.DecimalNumber` (import `contract/sitx`) so inbound numbers parse and quoted strings are tolerated; `offerReqToProto` uses `.Decimal.String()` for the proto string fields.
  - `protoOfferToJSON`: emit amounts as numbers — wrap with a helper `numJSON(s string) json.RawMessage { return json.RawMessage(s) }` (validate `s` is a decimal first; default `"0"`), e.g. `"pricePerUnit": gin.H{"amount": numJSON(o.GetPricePerStock()), "currency": o.GetCurrency()}`. `gin`/`encoding/json` renders `json.RawMessage` verbatim.
  - `peer_otc_initiate_handler.go`: the outbound `map[string]interface{}` builds `"amount": req.PricePerUnit.Amount`. If `req.PricePerUnit.Amount` is a string field, change the request struct's money `Amount` to `sitx.DecimalNumber` so it both parses client input and re-serializes as a number to the peer.
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Lint + commit**

```bash
cd api-gateway && golangci-lint run ./... && cd ..
git add api-gateway/internal/handler/peer_otc_handler.go api-gateway/internal/handler/peer_otc_initiate_handler.go api-gateway/internal/handler/peer_otc_handler_test.go
git commit -m "feat(sitx): OTC monetary amounts serialize as JSON numbers"
```

---

### Task 16: stock-service accept-posting composition uses enriched form (§6.8 note)

**Files:**
- Modify: `stock-service/internal/handler/peer_otc_grpc_handler.go` (`AcceptNegotiation` 4-posting composition)
- Test: `stock-service/internal/handler/peer_otc_grpc_handler_extra_test.go`

The accept composes 4 postings then calls `InitiateOutboundTxWithPostings`. Those postings flow into the spec wire via Task 12. Ensure each carries `account_type`/`asset_type` and the correct **internal** direction so that, after `InternalPostingToSpec`, the wire shows: Buyer→negative premium (credit), Seller→positive premium (debit), Buyer→positive option (debit one contract), Seller→negative option (credit one contract). Map per §3.6.

- [ ] **Step 1: Test** — `AcceptNegotiation` for a cross-bank offer produces 4 `SiTxPosting`s with: `[0]` buyer premium `AccountType` ACCOUNT/PERSON + `AssetType` MONAS + `Direction` DEBIT (buyer pays); `[1]` seller premium MONAS CREDIT; `[2]` seller option `AssetType` OPTION; `[3]` buyer option OPTION. Assert types/directions.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** — set `AccountType`/`AssetType` on the composed postings (read the current composition at `peer_otc_grpc_handler.go:776`). Keep the existing account-id resolution (`buyerAccountNumber`/participant id). Verify the direction constants match the intended economic effect (buyer pays premium → buyer outgoing → internal DEBIT).
- [ ] **Step 4: Run, verify pass.** Run `cd stock-service && go test ./...`
- [ ] **Step 5: Lint + commit**

```bash
cd stock-service && golangci-lint run ./... && cd ..
git add stock-service/internal/handler/peer_otc_grpc_handler.go stock-service/internal/handler/peer_otc_grpc_handler_extra_test.go
git commit -m "feat(sitx): accept-flow postings carry account/asset type tags for spec wire"
```

---

## Phase 8 — Conformance fixtures, integration, docs

### Task 17: Byte-level conformance fixtures (§8)

**Files:**
- Create: `contract/sitx/testdata/coffee_newtx.json`, `vote_no.json`, `public_stock.json`, `user.json`
- Create: `contract/sitx/conformance_test.go`

- [ ] **Step 1:** Write `conformance_test.go` that marshals the canonical Go values (the §2.8 coffee NEW_TX `Message`, a NO vote, a `PublicStocksResponse`, a `UserInformation`) and asserts each equals the bytes in the corresponding `testdata/*.json` (after `json.Compact`). Author the `testdata` files by hand from the spec.
- [ ] **Step 2: Run, verify fail** (fixtures absent / mismatch).
- [ ] **Step 3:** Create the fixtures with the exact spec bytes.
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Commit**

```bash
git add contract/sitx/testdata/ contract/sitx/conformance_test.go
git commit -m "test(sitx): byte-level conformance fixtures (cohort interop guard)"
```

---

### Task 18: Integration workflow tests (§8)

**Files:**
- Modify/Create: `test-app/workflows/sitx_conformance_test.go`
- Reuse helpers: `test-app/workflows/helpers_test.go`, `cohort_dry_run_test.go`

- [ ] **Step 1:** Add workflow tests that exercise two in-process stacks (or the existing cohort dry-run harness):
  1. Cross-bank payment NEW_TX→COMMIT; assert the on-wire `/interbank` body is spec-shaped (capture via a proxy/recorder) and balances move correctly (validates inversion end-to-end).
  2. NO-vote path returns `{vote:"NO",reasons:[{reason,posting}]}`.
  3. OTC negotiate→accept; assert spec `OtcOffer` body and a successful 4-posting accept.
  4. `GET /public-stock` returns a bare array with `sellers[]`; `GET /user` returns display names.
- [ ] **Step 2: Run, verify fail** (or red where behavior not yet wired).
- [ ] **Step 3:** Implement using the shared helpers (do **not** inline Kafka/verification/client setup).
- [ ] **Step 4: Run, verify pass:** `cd test-app && go test ./workflows/ -run SITX -v`
- [ ] **Step 5: Commit**

```bash
git add test-app/workflows/sitx_conformance_test.go
git commit -m "test(sitx): integration coverage for spec wire conformance"
```

---

### Task 19: Documentation + spec/memory updates

**Files:**
- Modify: `Specification.md` (Sections 17/19/20/21 as relevant — interbank routes, message types, enum values, business rules)
- Modify: `docs/api/REST_API_v3.md` (cross-bank-protocol request/response shapes)
- Regenerate: `api-gateway/docs/` Swagger (`make swagger`)
- Modify: memory `project_celina5_sitx.md` (note conformance completed)

- [ ] **Step 1:** Update `Specification.md` to document the spec wire shapes now emitted (Posting unions, signed amount, Transaction metadata, Vote `{vote,reasons}`, Commit/Rollback ForeignBankId, `/public-stock` bare array, `/user` display names, money-as-number).
- [ ] **Step 2:** Update `docs/api/REST_API_v3.md` cross-bank sections to match.
- [ ] **Step 3:** Run `make swagger`; commit regenerated `api-gateway/docs/`.
- [ ] **Step 4:** Update the `project_celina5_sitx` memory pointer to record wire conformance done on this date.
- [ ] **Step 5: Build + full test + commit**

```bash
make build && make test
git add Specification.md docs/api/REST_API_v3.md api-gateway/docs/ ~/.claude/.../memory/project_celina5_sitx.md
git commit -m "docs(sitx): document spec wire conformance across Specification + REST + swagger"
```

---

## Self-review checklist (run before execution)

- **Spec coverage:** §6.1 (Task 2), §6.1a (Task 1, 15), §6.2 (Task 5, 8), §6.3 (Task 6, 9), §6.4 (Task 7), §6.5 (Task 11), §6.6 (Task 10), §6.7 (Task 4), §6.8a (Task 13), §6.8b (Task 14), §6.8c (Task 15), §6.8-accept (Task 16), §6.9 (Task 12), §3-conformance (Task 3), tests (Task 17, 18), docs (Task 19). ✔
- **No placeholders:** all code steps show code; deep-internal edits (Tasks 8/9/10/11/12/16) instruct reading the current function first and name the exact symbols/lines to change.
- **Type consistency:** `DecimalNumber`, `InternalPosting`, `SpecPostingToInternal`/`InternalPostingToSpec`, `IsBalanced`, `SiTxForeignBankId`, `fbIDToProto`, `specPostingsToProto` are used consistently across tasks.

## Sequencing & risk notes

- Phases 1–3 (Tasks 1–6) are pure `contract` changes with no runtime behavior — safe to land first; the rest of the monorepo keeps compiling because nothing imports the new symbols until Task 7+.
- After Task 4 (proto), services won't compile until their consumers are updated (Tasks 7–16). If you need green CI between commits, do Tasks 4→7→8 in close succession or on a short-lived branch, then land the rest.
- **Tasks 5 and 11 are the money-correctness and saga-correctness pivots.** Review each in isolation; run the full `transaction-service` suite after both.
- Hard cutover: once Task 7/12 land, peers must speak the spec dialect. Coordinate the flag-day with the cohort; Task 17 fixtures are the shared target.
