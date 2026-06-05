# Unified OTC SP-2a (unified data model) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Fold the three remote OTC stores (`remote_otc_offer`, `peer_otc_negotiation`, `peer_option_contract`) into the local `OTCOffer`/`OTCNegotiation`/`OptionContract` tables, distinguishing local vs remote by a **bank-scoped natural key** (`routing_number == own` ⇒ local), with `kind` derived FE-only — and guard every local money path so a remote row can never enter it. **No client-facing route changes.**

**Architecture:** Surrogate `uint64` PK kept for FK stability; add `RoutingNumber int64` (NOT NULL; local=own, remote=peer) + `NativeID *string` (NULL for local, peer's foreign id for remote) + `UNIQUE(routing_number, native_id)`. `RoutingNumber` is stamped on local rows by a `BeforeCreate` hook reading a package-level own-routing set once at startup. A runtime guard forbids registering a peer whose code/routing equals ours (collision/masquerade prevention). Local-only queries gain a `routing_number == own` filter. Remote ingestion (offer refresher + inbound `/cross-bank-protocol/*` webhooks) writes the unified tables. **Fresh start — no data migration** (not production); existing dev data is discarded.

**Tech Stack:** Go, GORM (Postgres prod / sqlite `:memory:` unit tests), gRPC, Gin gateway, `test-app/workflows`.

**Spec:** `docs/superpowers/specs/2026-06-05-unified-otc-sp2a-data-model-design.md`. **Umbrella:** `docs/superpowers/specs/2026-06-04-unified-otc-local-remote-umbrella-design.md`.

**Branch:** create `feature/unified-otc-sp2a` off `Development` before Task 1.

---

## Ordering rationale
1. **Collision guard first** (Task 1) — independent, makes the key safe before any remote row can land in a shared table.
2. **Natural-key columns + own-routing stamping** (Task 2) — schema + local identity, no behavior change yet.
3. **Money-path guards** (Task 3) — land BEFORE the fold so when remote rows arrive they're already excluded from local logic. Harmless while no remote rows exist.
4. **Fold offers / negotiations / contracts** (Tasks 4–6) — repoint ingestion + SP-1 reads + inbound webhooks to the unified tables; retire the mirror tables/repos.
5. **Derive kind + ingestion guards + startup assertion** (Task 7).
6. **Final: dead-code sweep, docs, build/lint/test** (Task 8).

---

## Task 1: Runtime peer-collision guard

**Files:**
- Modify: `transaction-service/internal/handler/peer_bank_admin_grpc_handler.go` (struct + constructor + `CreatePeerBank` + `UpdatePeerBank`)
- Modify: `transaction-service/cmd/main.go` (pass `cfg.OwnBankCode` to the handler)
- Test: `transaction-service/internal/handler/peer_bank_admin_grpc_handler_test.go`

- [ ] **Step 1: Write the failing test.** Add to `peer_bank_admin_grpc_handler_test.go`. NOTE: the existing `newAdminTestHandler(t)` builds the handler with only the repo — update it (and all its callers) to pass an own-bank-code; use `"111"` to match the test default.

```go
func TestPeerBankAdmin_RejectsOwnCode(t *testing.T) {
	h := newAdminTestHandler(t) // now built with ownBankCode "111"
	ctx := context.Background()
	// same bank_code as own
	_, err := h.CreatePeerBank(ctx, &transactionpb.CreatePeerBankRequest{
		BankCode: "111", RoutingNumber: 222, BaseUrl: "http://x/api/v3", ApiToken: "t", Active: true,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("same bank_code: want InvalidArgument, got %v", err)
	}
	// same routing_number as own (111)
	_, err = h.CreatePeerBank(ctx, &transactionpb.CreatePeerBankRequest{
		BankCode: "222", RoutingNumber: 111, BaseUrl: "http://x/api/v3", ApiToken: "t", Active: true,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("same routing: want InvalidArgument, got %v", err)
	}
	// a genuinely different peer still succeeds
	if _, err := h.CreatePeerBank(ctx, &transactionpb.CreatePeerBankRequest{
		BankCode: "222", RoutingNumber: 222, BaseUrl: "http://x/api/v3", ApiToken: "t", Active: true,
	}); err != nil {
		t.Fatalf("distinct peer: %v", err)
	}
}
```
Update `newAdminTestHandler` to: `return handler.NewPeerBankAdminGRPCHandler(repository.NewPeerBankRepository(db), "111")`.

- [ ] **Step 2: Run — expect FAIL** (compile error: constructor arity).
`cd "/Users/lukasavic/Desktop/Faks/Softversko inzenjerstvo/EXBanka-1-Backend/transaction-service" && go test ./internal/handler/ -run TestPeerBankAdmin -v`

- [ ] **Step 3: Add own-code to the handler.** In `peer_bank_admin_grpc_handler.go`: add `ownBankCode string` + `ownRouting int64` fields to the handler struct; change the constructor to `NewPeerBankAdminGRPCHandler(repo *repository.PeerBankRepository, ownBankCode string) *PeerBankAdminGRPCHandler` and set `ownBankCode` + `ownRouting, _ = strconv.ParseInt(ownBankCode, 10, 64)` (add `strconv` import). At the top of `CreatePeerBank` (after the required-fields check) and `UpdatePeerBank` (only relevant if routing/bank-code becomes updatable — current Update doesn't change them, so guard `CreatePeerBank` only; add a defensive guard in Update only if it ever sets bank_code/routing):

```go
	if req.GetBankCode() == h.ownBankCode || req.GetRoutingNumber() == h.ownRouting {
		return nil, status.Error(codes.InvalidArgument, "peer bank_code/routing must differ from this bank's own")
	}
```

- [ ] **Step 4: Wire own-code in main.go.** In `transaction-service/cmd/main.go` where the handler is constructed (`NewPeerBankAdminGRPCHandler(...)`), pass `cfg.OwnBankCode`.

- [ ] **Step 5: Run — expect PASS.** `go test ./internal/handler/ -run TestPeerBankAdmin -v`; also `go build ./...`.

- [ ] **Step 6: Commit.**
```bash
git add transaction-service/internal/handler/peer_bank_admin_grpc_handler.go transaction-service/cmd/main.go transaction-service/internal/handler/peer_bank_admin_grpc_handler_test.go
git commit -m "feat(otc): reject peer bank registration colliding with own code/routing (SP-2a)"
```
(End every commit with the `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>` trailer.)

---

## Task 2: Bank-scoped natural-key columns + own-routing stamping

**Files:**
- Modify: `stock-service/internal/model/otc_offer.go`, `otc_negotiation.go`, `option_contract.go`
- Create: `stock-service/internal/model/own_routing.go`
- Modify: `stock-service/cmd/main.go` (set own routing at startup; make local FK columns nullable is in the model files)
- Test: `stock-service/internal/model/own_routing_test.go`

- [ ] **Step 1: Own-routing holder + BeforeCreate stamping.** Create `stock-service/internal/model/own_routing.go`:

```go
package model

import (
	"strconv"
	"sync/atomic"
)

// ownRouting is this bank's routing number, set once at startup from
// OWN_BANK_CODE. Local OTC rows are stamped with it by BeforeCreate hooks so
// that local-vs-remote is `routing_number == ownRouting`. It is process-wide
// identity (not per-request), so a package-level value (set once) is the
// least-invasive place for it — avoids threading config through every service
// and create call site.
var ownRouting atomic.Int64

// SetOwnRouting is called once at startup (main.go) with the parsed
// OWN_BANK_CODE. Safe no-op if bankCode isn't numeric.
func SetOwnRouting(bankCode string) {
	if n, err := strconv.ParseInt(bankCode, 10, 64); err == nil {
		ownRouting.Store(n)
	}
}

// OwnRouting returns the configured own routing number.
func OwnRouting() int64 { return ownRouting.Load() }
```

- [ ] **Step 2: Add columns + BeforeCreate to the three models.**
In `otc_offer.go` `OTCOffer` struct, add (after `ID`):
```go
	RoutingNumber int64   `gorm:"not null;default:0;uniqueIndex:ux_otc_offer_native,priority:1" json:"routing_number"`
	NativeID      *string `gorm:"size:128;uniqueIndex:ux_otc_offer_native,priority:2" json:"native_id,omitempty"`
```
Change `StockID uint64 ... not null ...` to `StockID *uint64` (nullable — remote offers have no local stock); change `InitiatorAccountID uint64 ... not null;default:0` — keep (already defaults 0; remote rows leave 0). Add a `BeforeCreate` hook:
```go
func (o *OTCOffer) BeforeCreate(tx *gorm.DB) error {
	if o.RoutingNumber == 0 {
		o.RoutingNumber = OwnRouting()
	}
	return nil
}
```
Mirror the same pattern in `OTCNegotiation` (`ux_otcneg_native`; make `ParentOfferID` → `*uint64` nullable since remote negotiations have no local parent offer row) and `OptionContract` (`ux_oc_native`; make `OfferID` → `*uint64` nullable — drop its `uniqueIndex` tag since remote contracts share no local OfferID; keep `StockID` → `*uint64`). Add a `BeforeCreate` to each that stamps `RoutingNumber = OwnRouting()` when 0. **If a model already has a `BeforeCreate`, merge the stamping into it.**

> Any code that dereferenced `StockID`/`ParentOfferID`/`OfferID` as a value now must handle the pointer. Grep each (`grep -rn "\.StockID" stock-service/internal | grep -i otc`; same for ParentOfferID/OfferID) and adjust local code paths to `*x.StockID` (they're always set for local rows). The money-path guards (Task 3) ensure only local rows reach these.

- [ ] **Step 3: Set own routing at startup.** In `stock-service/cmd/main.go`, immediately after config load (before AutoMigrate), add `model.SetOwnRouting(cfg.OwnBankCode)`.

- [ ] **Step 4: Stamp routing in the three local create paths.** Belt-and-suspenders with the hook (the hook already sets it from `OwnRouting()`, so no per-call change is strictly needed) — verify the hook fires by test rather than editing each call site. Test `stock-service/internal/model/own_routing_test.go`:
```go
package model

import "testing"

func TestBeforeCreate_StampsOwnRouting(t *testing.T) {
	SetOwnRouting("111")
	o := &OTCOffer{}
	if err := o.BeforeCreate(nil); err != nil {
		t.Fatal(err)
	}
	if o.RoutingNumber != 111 {
		t.Fatalf("offer routing = %d, want 111", o.RoutingNumber)
	}
	// a pre-set (remote) routing is preserved
	r := &OTCOffer{RoutingNumber: 222}
	_ = r.BeforeCreate(nil)
	if r.RoutingNumber != 222 {
		t.Fatalf("remote routing overwritten: %d", r.RoutingNumber)
	}
}
```
(Add equivalent assertions for `OTCNegotiation` + `OptionContract`.)

- [ ] **Step 5: Build + test.** `cd stock-service && go build ./... && go test ./internal/model/ -run 'BeforeCreate|OwnRouting' -v`. Fix any pointer-deref compile errors surfaced by Step 2.

- [ ] **Step 6: Commit.**
```bash
git add stock-service/internal/model/ stock-service/cmd/main.go
git commit -m "feat(otc): bank-scoped natural key (routing_number, native_id) + own-routing stamping (SP-2a)"
```

---

## Task 3: Money-path guards (`routing_number == own` on local-only queries)

**Files:**
- Modify: `stock-service/internal/repository/otc_offer_repository.go`, `otc_negotiation_repository.go` (inject `ownRouting`; add the filter)
- Modify: `stock-service/cmd/main.go` (repo construction passes own routing — or read `model.OwnRouting()`)
- Test: `stock-service/internal/repository/otc_guard_test.go`

The local-only query sites (verified): offer `LockByIDTx`/`ListOpenForCache`/`ListExpiringOffers`; negotiation `findChainByBidder`/`LockByID`/`ListOpenByParentOfferForUpdate`/`ListByBidder`/`ListByParentOffer`. Each must exclude rows where `routing_number != OwnRouting()`.

- [ ] **Step 1: Write the failing test** `stock-service/internal/repository/otc_guard_test.go`: seed one local offer (`routing=111`) and one remote offer (`routing=222`) in `OTCOffer`; assert `ListOpenForCache` and `ListExpiringOffers` return ONLY the local one; `LockByIDTx(remoteID)` returns not-found. Seed a local + remote `OTCNegotiation`; assert `ListByBidder`/`ListByParentOffer`/`ListOpenByParentOfferForUpdate` exclude the remote row. Use sqlite `:memory:` + `model.SetOwnRouting("111")`.

- [ ] **Step 2: Run — expect FAIL** (remote rows currently returned).

- [ ] **Step 3: Add the guard.** Simplest, lowest-blast: add `.Where("routing_number = ?", model.OwnRouting())` to each local-only query method body (they're all in the two repo files). Examples:
  - `ListOpenForCache`: `r.db.Where("status IN ? AND counterparty_owner_id IS NULL AND routing_number = ?", openStatuses, model.OwnRouting())`.
  - `ListExpiringOffers`: add `AND routing_number = ?`.
  - `LockByIDTx`: after the `First(&o, id)`, if `o.RoutingNumber != model.OwnRouting()` return `gorm.ErrRecordNotFound` (don't lock a remote row in a local TX).
  - `findChainByBidder`, `LockByID`, `ListOpenByParentOfferForUpdate`, `ListByBidder`, `ListByParentOffer`: add `.Where("routing_number = ?", model.OwnRouting())` (for `LockByID`, the post-fetch routing check like `LockByIDTx`).
  Using `model.OwnRouting()` directly avoids changing repo constructors.

- [ ] **Step 4: Run — expect PASS.** `go test ./internal/repository/ -run Guard -v`. Also re-run the existing OTC repo/service/handler suites to confirm no local behavior changed: `go test ./internal/repository/ ./internal/service/ ./internal/handler/`.

- [ ] **Step 5: Commit.**
```bash
git add stock-service/internal/repository/ stock-service/internal/repository/otc_guard_test.go
git commit -m "fix(otc): guard local-only OTC queries with routing_number==own so remote rows never enter local money paths (SP-2a)"
```

---

## Task 4: Fold offers into `OTCOffer` (retire `remote_otc_offer`)

**Files:**
- Modify: `stock-service/internal/repository/otc_offer_repository.go` (add remote upsert/reconcile/get-by-surrogate scoped to remote rows)
- Modify: `stock-service/internal/otccache/option_cache.go` (refresher writes `OTCOffer` remote rows via the offer repo)
- Modify: `stock-service/internal/handler/otc_options_handler.go` (`resolveRemoteOffer` reads `OTCOffer` remote rows)
- Delete: `stock-service/internal/model/remote_otc_offer.go`, `stock-service/internal/repository/remote_otc_offer_repository.go` (+ its test)
- Modify: `stock-service/cmd/main.go` (remove `RemoteOTCOffer` from AutoMigrate + the `remoteOfferRepo` wiring; wire the offer repo's remote methods into the refresher + GetOffer handler)

- [ ] **Step 1: Add remote methods to `OTCOfferRepository`** mirroring the retired `RemoteOTCOfferRepository`, but on `OTCOffer` rows keyed by `(routing_number, native_id)`:
  - `UpsertRemote(o *model.OTCOffer, seenAt time.Time) (uint64, error)` — `clause.OnConflict{Columns: routing_number+native_id, DoUpdates: ...}`, sets status `open`, stamps a `LastSeenAt` (add a `LastSeenAt *time.Time` column to `OTCOffer` for remote reconciliation; nullable, local rows leave nil). Returns surrogate id.
  - `ReconcileRemoteNotSeen(peerRouting int64, seenNativeIDs []string) (int64, error)` — `SkipHooks` bulk flip of `routing_number=peerRouting AND status='open' AND native_id NOT IN ?` to `cancelled`. (peerRouting is never own — guaranteed by Task 1.)
  - `GetRemoteByID(id uint64) (*model.OTCOffer, error)` — `First(&o, id)`; return not-found if `o.RoutingNumber == model.OwnRouting()` (a local id isn't a "remote offer").
  The refresher maps a peer offer into an `OTCOffer` remote row: `RoutingNumber=peerRouting`, `NativeID=&foreignOfferID`, `Ticker/StrikePrice/Premium/SettlementDate/Status`, `StockID=nil`, `InitiatorOwnerType=OwnerBank`/seller as appropriate (store the SI-TX seller id in `InitiatorBankCode`/a field the read shaping uses for `seller_id`). Reuse the field mapping the SP-1 `RemoteOTCOffer` carried (seller id, currencies). Add the currency columns to `OTCOffer` if not present (`StrikeCurrency`/`PremiumCurrency` — SP-1's mirror had them; `OTCOffer` derives currency from the stock for local, so add nullable `*string` columns used only for remote rows).

- [ ] **Step 2: Repoint the refresher.** In `option_cache.go` `buildAndMirrorRemoteOffers`, change the `RemoteOfferMirror` interface to the new offer-repo methods (`UpsertRemote`/`ReconcileRemoteNotSeen`) and map into `model.OTCOffer` instead of `model.RemoteOTCOffer`. The `LocalID` stamped onto the cache row is the surrogate id returned by `UpsertRemote`.

- [ ] **Step 3: Repoint GetOffer remote-resolve.** In stock-service `OTCOptionsHandler.resolveRemoteOffer`, call `OTCOfferRepository.GetRemoteByID` instead of `RemoteOTCOfferRepository.GetByID`; map the `OTCOffer` remote row into the `OTCOfferResponse` (kind="remote" derived from `RoutingNumber != OwnRouting()`).

- [ ] **Step 4: Delete the mirror model + repo** and remove `&model.RemoteOTCOffer{}` from `cmd/main.go` AutoMigrate + the `remoteOfferRepo := repository.NewRemoteOTCOfferRepository(db)` line (replace its uses with the `OTCOfferRepository`). `grep -rn "RemoteOTCOffer" stock-service/` must return nothing after.

- [ ] **Step 5: Build + tests.** `go build ./... && go test ./internal/otccache/ ./internal/handler/ ./internal/repository/`. Update the SP-1 offer tests that referenced `RemoteOTCOffer*` to the new offer-repo remote methods. Lint clean.

- [ ] **Step 6: Commit.**
```bash
git add stock-service/ && git rm stock-service/internal/model/remote_otc_offer.go stock-service/internal/repository/remote_otc_offer_repository.go stock-service/internal/repository/remote_otc_offer_repository_test.go
git commit -m "refactor(otc): fold remote offers into OTCOffer (remote rows); retire remote_otc_offer (SP-2a)"
```

---

## Task 5: Fold negotiations into `OTCNegotiation` (retire `peer_otc_negotiation`)

**Files:**
- Modify: `stock-service/internal/repository/otc_negotiation_repository.go` (add remote-row methods)
- Modify: `stock-service/internal/handler/peer_otc_grpc_handler.go` (every `negRepo.*` call repointed to `OTCNegotiation` remote rows)
- Modify: `stock-service/internal/handler/otc_negotiation_handler.go` (SP-1 read-merge `peerNegToProto`/`ListByClient` → query unified table for remote rows)
- Delete: `stock-service/internal/model/peer_otc_negotiation.go`, `stock-service/internal/repository/peer_otc_negotiation_repository.go`
- Modify: `stock-service/cmd/main.go` (remove from AutoMigrate + wiring)

A remote `OTCNegotiation` row: `RoutingNumber`=issuing bank routing, `NativeID=&foreignID`, `BidderBankCode`/`BidderOwnerType=OwnerBank` or the SI-TX buyer/seller stored in the existing cross-bank columns, terms from the SI-TX offer, `ParentOfferID=nil`, status. Store the SI-TX `OfferJSON`, buyer/seller routing+id, and cascade lot key in columns added to `OTCNegotiation` for remote rows (nullable): `RemoteOfferJSON *string`, `BuyerRoutingNumber *int64`, `BuyerSITXID *string`, `SellerRoutingNumber *int64`, `SellerSITXID *string`, `ParentOfferRouting *int64`, `ParentOfferNativeID *string`. (These mirror the retired `PeerOtcNegotiation` fields.)

- [ ] **Step 1:** Add remote-row methods to `OTCNegotiationRepository` replacing the `PeerOtcNegotiationRepository` API, scoped to remote rows (`routing_number != OwnRouting()`): `UpsertRemote`, `GetRemoteByPeerAndForeignID(peerRouting int64, nativeID string)`, `UpdateRemoteOffer`, `UpdateRemoteStatus`, `CompareAndSetRemoteStatus`, `ListRemoteBySellerAndParent`, `ListRemoteByClient`, `GetRemoteByForeignID`. Each keys on `(routing_number, native_id)` and filters `routing_number != OwnRouting()`.

- [ ] **Step 2:** Repoint every `h.negRepo.*` call in `peer_otc_grpc_handler.go` (the inbound webhook handlers `CreateNegotiation`/`UpdateNegotiation`/`DeleteNegotiation`/`AcceptNegotiation`/`CascadeCancelSiblings`/`RecordOutboundNegotiation`/`MarkNegotiationAccepted`/`ListMyPeerNegotiations`/`ValidatePeerOptionMoneyLeg`) to the new remote methods on `OTCNegotiationRepository`. The wire is unchanged; only the persistence target changes. **Apply the ingestion collision guard (Task 7) here too** (reject inbound where claimed routing == own).

- [ ] **Step 3:** Repoint the SP-1 read-merge: `otc_negotiation_handler.go` `peerNegToProto` + the `PeerNegotiationLister`/`ListByClient` used by `ListMyNegotiations`/history now read remote `OTCNegotiation` rows (via `ListRemoteByClient`). Because local + remote now live in one table, the merge can become a single query (`ListByBidder` for local + the remote rows) — but keep the existing two-call shape if simpler; the key change is the remote source.

- [ ] **Step 4:** Delete `peer_otc_negotiation.go` + its repo; remove from AutoMigrate + wiring. `grep -rn "PeerOtcNegotiation" stock-service/` returns nothing.

- [ ] **Step 5:** Build + tests (handler + service + repo + the inbound-webhook tests). Update tests referencing `PeerOtcNegotiation*`. Lint clean.

- [ ] **Step 6: Commit.**
```bash
git add stock-service/ && git rm stock-service/internal/model/peer_otc_negotiation.go stock-service/internal/repository/peer_otc_negotiation_repository.go
git commit -m "refactor(otc): fold cross-bank negotiations into OTCNegotiation (remote rows); retire peer_otc_negotiation (SP-2a)"
```

---

## Task 6: Fold contracts into `OptionContract` (retire `peer_option_contract`)

**Files:** analogous to Task 5, for `peer_option_contract` → `OptionContract`.
- Modify: `stock-service/internal/repository/option_contract_repository.go` (remote methods: `UpsertRemoteIdempotent`, `GetRemoteByNegotiationAndDirection`, `GetRemoteByID`, `ListRemoteByLocalParticipant`, `ListRemoteExpiring`), keyed on `(routing_number, native_id)` where `native_id = crossbank_tx_id + ":" + posting_index` (preserve the retired table's natural key inside native_id) and `routing_number != own`. Add nullable remote columns to `OptionContract`: `CrossbankTxID` (exists), `PostingIndex *int32`, `NegotiationRoutingNumber *int64`, `NegotiationNativeID *string`, `Direction *string` (CREDIT/DEBIT), plus reuse existing buyer/seller bank-code + qty/strike/currency/settlement fields.
- Modify: `peer_otc_grpc_handler.go` `RecordOptionContract`/`InitiateOptionExercise`/`LookupPeerOptionContract`/`ValidatePeerOptionMoneyLeg` + the OTC expiry cron's `WithPeerContracts` path → unified table.
- Modify: SP-1 contract read-merge (`ListMyContracts`/`GetContract` `resolveRemoteContract` + `ListByLocalParticipant`) → unified remote rows.
- Delete: `peer_option_contract.go` + repo; remove from AutoMigrate + wiring.

- [ ] **Step 1–6:** same TDD shape as Task 5 (remote methods → repoint webhooks/exercise → repoint reads → delete model+repo → build/test/lint → commit). Commit message: `refactor(otc): fold cross-bank contracts into OptionContract (remote rows); retire peer_option_contract (SP-2a)`.

---

## Task 7: Derive `kind` from routing + ingestion guards + startup assertion

**Files:**
- Modify: stock-service read shaping (wherever `kind` was set from source/`kind` column) → derive `kind = RoutingNumber == OwnRouting() ? "local" : "remote"`.
- Modify: the offer refresher + inbound webhook handlers — reject/skip any payload whose claimed routing == own.
- Modify: `stock-service/cmd/main.go` — startup assertion.

- [ ] **Step 1:** Ensure every place that produces the FE `kind`/provenance derives it from `RoutingNumber` (grep for hard-coded `Kind: "remote"`/`"local"` in the otccache + handlers; replace with the derivation helper). Add a tiny helper `func kindFor(routing int64) string { if routing == model.OwnRouting() { return "local" }; return "remote" }` and use it.

- [ ] **Step 2:** Ingestion guards (defense-in-depth): in the offer refresher's remote mapping and in each inbound `/cross-bank-protocol/*` write handler, if the claimed routing/bank-code == own, log WARN and skip/reject (return `codes.InvalidArgument` for the inbound RPCs). Test: an inbound `CreateNegotiation` claiming our own routing is rejected; a refresher peer offer claiming own routing is skipped.

- [ ] **Step 3:** Startup assertion in `cmd/main.go`: after wiring the peer-admin client, list peers; if any has `routing == OwnRouting()`, `log.Printf("ERROR: peer bank %s collides with own routing; cross-bank ingestion disabled", ...)` and skip starting the refresher/inbound ingestion (or `log.Fatalf` if simpler — acceptable per spec).

- [ ] **Step 4:** Build + tests + lint. Commit: `feat(otc): derive kind from routing; ingestion collision guards + startup assertion (SP-2a)`.

---

## Task 8: Dead-code sweep, docs, full verification

- [ ] **Step 1: Dead-code sweep.** `grep -rn --include="*.go" "RemoteOTCOffer\|PeerOtcNegotiation\|PeerOptionContract\|remote_otc_offer\|peer_otc_negotiation\|peer_option_contract" .` — only allowed hits: the inbound `/cross-bank-protocol` wire field names (string literals) and DB table names if intentionally preserved; NO Go type/repo references. `golangci-lint run ./...` in stock-service + api-gateway + transaction-service — zero `unused`/`deadcode` findings on touched files.
- [ ] **Step 2: Docs.** `Specification.md`: update the OTC entities (one table per entity; bank-scoped key; collision invariant) and remove the retired-table entries. `docs/api/REST_API_v3.md`: no route changes; note the collision rejection on `POST /api/v3/peer-banks` and that provenance/`kind` derives from routing. `make swagger`.
- [ ] **Step 3: Version.** Bump `VERSION` MINOR (`api-gateway/internal/version/version.go` in sync) — the peer-banks 400 on collision + the internal consolidation; current is 1.11.3 → `1.12.0`.
- [ ] **Step 4: Full verify (real output).** `make build 2>&1 | tail -20`, `make lint 2>&1 | tail -30`, `make test 2>&1 | tail -40`. All clean. Add a `test-app/workflows` integration test: registering a peer with our own bank code returns 400; cross-bank offer discovery still surfaces remote offers with `kind:"remote"` from the unified table (skip the two-stack parts cleanly when no peer).
- [ ] **Step 5: Commit.** `test+docs(otc): SP-2a dead-code sweep, docs, version, integration tests`.

---

## Self-review notes
- **Spec coverage:** §2 identity → Task 2; §3 collision (registration/ingestion/startup) → Tasks 1, 7; §4 fold+retire → Tasks 4–6 (no migration, fresh start); §5 money-path guards → Task 3; §6 repoint reads+webhooks → Tasks 4–6; §9 removed/retired → Tasks 4–6 + Task 8 sweep. No client route changes (SP-2b).
- **Key decision baked in:** `NativeID` nullable (NULL local / foreign-id remote); `RoutingNumber` stamped by `BeforeCreate` from the package-level own-routing; concurrency-safe (no per-create surrogate-id-in-unique-index race).
- **Highest risk = Task 3 guards + Tasks 4–6 webhook repointing.** Each guarded query and each repointed webhook has a test asserting unchanged observable behavior + no remote-row leak into local logic.
- **Open implementation detail for review at execution:** exact set of nullable remote-only columns added to `OTCNegotiation`/`OptionContract` (Steps 5/6) — keep them minimal (only what the retired tables carried).
