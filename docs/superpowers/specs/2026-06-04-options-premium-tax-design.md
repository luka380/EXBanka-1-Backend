# Options & Premium Tax — Obračun poreza extension (resolution-month model)

**Date:** 2026-06-04
**Status:** Approved (design) — pending spec review
**Sub-project:** SP1 of the TODO_final backlog (the only genuinely new requirement; SP2–SP6 are gap-fills tracked separately).

## 1. Requirement (verbatim intent)

Extend the existing 15% capital-gains tax (`Obračun poreza`) to OTC option premiums, exercises, and expiries.

1. **Seller receives premium (OTC).** Premium income is taxable. `tax = 15% × premium`.
   *Example:* premium $1150 → tax $172.50.
2. **Buyer exercises the option (OTC).** Buyer buys shares at strike; market price is higher; the bargain element minus the premium is taxable.
   `taxable = (market − strike) × qty − premium`; `tax = 15% × taxable`.
   *Example:* 50 AAPL, strike $200, market $250, premium $1150 → taxable `(250−200)×50 − 1150 = 1350` → tax $202.50.
3. **Option expires unexercised (OTC).**
   - Buyer: the lost premium is a **loss that reduces total capital gain for that month**.
   - Seller: no additional tax (premium was already taxed when received).
4. **Exception — actuaries trading on behalf of the bank** do **not** pay the 15%. Their option profit (premiums included) flows to **Profit Banke**, identical to the dividend rule.

## 2. What already exists (so we don't rebuild it)

The capital-gain → monthly tax-collection machinery lives in `stock-service`:

- `model.CapitalGain` — one row per realized gain/loss. Fields used here: `OwnerType` (`client`|`bank`), `OwnerID` (`nil` for bank), `OTC bool`, `SecurityType` (`stock`|`option`), `Ticker`, `Quantity`, `BuyPricePerUnit`, `SellPricePerUnit`, `TotalGain` (signed), `Currency`, `AccountID`, `TaxYear`/`TaxMonth`, `TaxCollectionID` (`NULL` = uncollected), `IdempotencyKey` (deterministic, for saga delete-on-rollback).
- `TaxService.CollectTax(year, month)` — taxes the **sum of positive uncollected gains** per `(owner, account, currency)` at 15%; cron monthly + manual RPC.
- OTC **accept** saga (`otc_accept_saga.go`): records seller `+premium` and buyer `−premium` option rows.
- OTC **exercise** saga (`otc_exercise_saga.go`): records seller strike gain `(strike − sellerCostBasis)×qty`; the **buyer step is a no-op** today.
- OTC **expiry** cron (`otc_expiry_cron.go`): releases the seller reservation, flips status; records **no** capital gain.
- Cross-bank (SI-TX) exercise (`peer_otc_grpc_handler.go recordOptionExercise`): seller side records strike gain; **buyer side records no gain**; buyer shares credited with cost basis = strike.

### The four current gaps vs the requirement

| Requirement | Current behaviour | Gap |
|---|---|---|
| Seller premium taxed at accept | ✅ recorded `+premium` at accept | none |
| Buyer exercise `(mkt−strike)×qty − premium` in exercise month | buyer exercise step is a **no-op**; premium booked at **accept**, not exercise | **record buyer exercise gain; move premium to resolution; step up basis to market** |
| Buyer expiry: `−premium` loss in **expiry** month | premium booked at **accept**, expiry records nothing | **book `−premium` at expiry, not accept** |
| Bank (actuary-on-behalf) exempt → Profit Banke | `ListOwnersWithGains` includes `owner_type='bank'` → **bank gains are currently swept into collection** | **exclude bank owners from collection** |

## 3. Chosen model — "resolution-month, exercise-time taxation"

**Principle:** the seller's premium income is realized at accept (taxed then). The **buyer's** premium is only "spent" when the option **resolves** (exercise or expiry) and lands in the resolution month. The buyer is taxed on the bargain element **at exercise**, and their acquired shares' cost basis **steps up to market** so the same appreciation is never taxed twice.

### 3.1 Lifecycle (intra-bank)

| Event | Seller capital-gain row | Buyer capital-gain row | Buyer holding basis |
|---|---|---|---|
| **Accept** | `+premium` (`option`, OTC) — *unchanged* | **none** (was `−premium`; now removed) | — |
| **Exercise** | `+(strike − sellerBasis)×qty` (`stock`) — *unchanged* | **`+((market − strike)×qty − premium)`** (`option`, OTC) — *new* | **market** (was strike) |
| **Expiry** | none — *unchanged* | **`−premium`** (`option`, OTC) — *new*, in expiry month | — |

`market` = current listing price of the underlying (`Listing.Price`) snapshotted at exercise.

**Why basis steps up to market:** the buyer is taxed on `(market−strike)` at exercise. If their basis stayed at strike, selling later at market would re-tax `(market−strike)` as a stock gain — double taxation. Stepping basis to market makes a later sale-at-market produce zero gain, matching the requirement example exactly. This changes the prior "shares acquired as if bought at strike" invariant; that is intended.

**Net-tax equivalence check (buyer who exercises then sells at price S):**
`((market−strike)×qty − premium)` [exercise] `+ (S−market)×qty` [sale] `= (S−strike)×qty − premium`. Correct, no double count.

### 3.2 Bank exemption (Profit Banke)

All bank-owned capital-gain rows (`owner_type='bank'`, the actuary-on-behalf-of-bank case — premiums, exercise gains, stock gains, dividends) are **excluded from `CollectTax`**. They remain recorded for audit/portfolio but are never taxed, so the profit stays with the bank. This is the same treatment dividends already require and fixes the existing over-collection.

### 3.3 Premium-timing consequence (called out explicitly)

If an option is accepted in one month and exercised/expired in a later month, the buyer's premium effect lands in the **later** (resolution) month, while the seller's premium income stays in the **accept** month. This is the literal reading of the requirement's "reduces capital gain for that month" and was explicitly chosen.

## 4. Implementation plan — intra-bank (high confidence)

All saga edits **preserve the saga shape** (same steps, same `StepKind` names) — only step *bodies* change — so crash-recovery (`saga_recovery.go`, which rebuilds the saga from `(sagaID, contractID)` and re-drives persisted steps) is unaffected. This is the key safety property given the saga's fragility.

**C1 — Accept saga (`otc_accept_saga.go`).** Turn `StepRecordBuyerPremiumCost`'s `Forward`/`Backward` into no-ops (return `nil`). Keep the step in place (shape unchanged). Leave `StepRecordSellerPremiumGain` untouched. Add a code comment pointing to this spec.

**C2 — Exercise saga (`otc_exercise_saga.go`).**
- Pre-saga, alongside the existing `sellerCostBasis` snapshot, fetch the underlying market price via `s.stockMeta.GetListingBySecurityIDAndType(c.StockID,"stock").Price`. Snapshot into `marketPriceKnown`/`marketPrice`. A lookup failure → log + skip the buyer row only (never block the exercise; money safety first). Convert market price to `c.StrikeCurrency` if the listing currency differs.
- Fill `StepRecordBuyerExerciseCost.Forward`: compute `premiumInStrikeCcy` (convert `c.PremiumPaid` from `c.PremiumCurrency` to `c.StrikeCurrency` if needed), `gain = market.Sub(strike).Mul(qty).Sub(premiumInStrikeCcy)`, and `capitalGainRepo.Create` a row: `OwnerType=c.BuyerOwnerType`, `OwnerID=c.BuyerOwnerID`, `OTC=true`, `SecurityType="option"`, `Ticker=c.Ticker`, `Quantity=qty`, `BuyPricePerUnit=strike`, `SellPricePerUnit=market`, `TotalGain=gain` (may be negative), `Currency=c.StrikeCurrency`, `AccountID=c.BuyerAccountID`, `TaxYear/TaxMonth = exercisedAt`, `IdempotencyKey = "<sagaID>:buyer-exercise-cg"`. `Backward`: `DeleteByIdempotencyKey`. Guard on `capitalGainRepo != nil && marketPriceKnown` (mirrors the seller-gain guard).
- Change `buyerHolding.AveragePrice` from `c.StrikePrice` to the snapshotted **market price** (basis step-up). If market price is unknown (lookup failed), fall back to strike (preserves today's behaviour for that degraded case).

**C3 — Expiry cron (`otc_expiry_cron.go`).**
- Wire a `*repository.CapitalGainRepository` into `OTCExpiryCron` (new optional field + `WithCapitalGains(...)` builder + constructor wiring in `cmd/main.go`; `nil` disables, so existing unit tests still pass).
- In `expireContract`, **before** the `Save`/status-flip, idempotently record the buyer's loss: `TotalGain = c.PremiumPaid.Neg()`, `SecurityType="option"`, `OTC=true`, `Currency=c.PremiumCurrency`, `AccountID=c.BuyerAccountID`, `TaxYear/TaxMonth = expiry time`, `IdempotencyKey = fmt.Sprintf("expire-contract-%d-buyer-premium-loss", c.ID)`. Ordering matters: insert-before-flip means a crash between insert and flip re-runs safely (idempotent insert; the cron re-selects `status=active` rows). Seller: nothing.

**C4 — Bank exemption (`tax_collection_repository.go ListOwnersWithGains`).** Add `baseQuery = baseQuery.Where("cg.owner_type = ?", "client")` so only client owners are ever returned for collection. `CollectTax`/`collectTaxInner` iterate only returned owners, so bank rows are never collected. (Double-checks: `SumUncollectedByOwnerMonth` is only invoked per returned owner; the cron's per-owner debit/credit never touches a bank owner.)

## 5. Cross-bank / SI-TX buyer taxation — DEFERRED (documented gap)

**Decision (2026-06-04, after code inspection):** cross-bank *buyer* option taxation is **deferred** as a documented gap. The requirement's examples are all intra-bank OTC, which §4 fully implements. Cross-bank *sellers* are already taxed (the existing strike-gain write in `recordOptionExercise` DEBIT path, `peer_otc_grpc_handler.go:1385-1408`, is unchanged by this work).

**Why deferred (two hard blockers, not a time constraint):**

1. **No premium available on the buyer's bank.** The SI-TX `OptionDescription` wire (`contract/sitx/otc_types.go`) is frozen (`feedback_interbank_protocol_frozen`) and carries only `PricePerUnit` (strike), `Amount` (qty), `SettlementDate`, `Stock` — **no premium**. The peer contract (`RecordOptionContract`, `:950`) is formed solely from that description, and the accept-time premium money leg is processed by transaction-service, never delivered to this handler. So `(market−strike)×qty − premium` is uncomputable: the `premium` term is unknown.
2. **No market-price resolver on the peer handler.** `PeerOTCGRPCHandler` has `holdings`, `capitalGainRepo`, `holdingReserver`, etc., but **no listing/price source**, so the `market` term is also unavailable without new wiring.

Building both (a local premium-persistence path that the frozen wire can't feed, plus a price resolver injected into the peer handler) would be fragile new infrastructure that still could not satisfy the requirement's formula. Per the codebase convention of leaving cross-bank edges explicitly open (`docs/Bugs.txt` §"Cohort-dependent / cross-team TODOs"), this is recorded there instead.

**Consequence:** a cross-bank buyer who exercises is currently taxed via their *eventual stock sale* (existing stock-CG mechanism on the shares credited at strike basis), not at exercise. This is strictly the pre-existing behaviour — this change does not regress it. Same-bank buyers (the requirement's scenario) get the full resolution-month treatment.

**Unblock path (future, needs cohort agreement):** add a `premium` field to the SI-TX `OptionDescription` (a wire change requiring the four-bank cohort to agree, per the frozen-protocol rule) **or** a side channel that delivers the premium to the buyer's bank, then inject a price resolver into the peer handler and mirror §4 C2/C3 in `recordOptionExercise` (CREDIT) and `expirePeerContract`.

## 6. Cutover / migration

Existing **ACTIVE** contracts accepted under the old model already have a buyer `−premium` row dated to the accept month. Under the new model the premium is booked at resolution, so those would double-count if such a contract is later exercised/expired. One-time cleanup at startup (idempotent): delete buyer-premium option rows (`SecurityType='option'`, `OTC=true`, `TotalGain<0`, `tax_collection_id IS NULL`) whose `OfferID/contract` is still `ACTIVE`. Already-collected rows are left untouched. This is a local-Development clean cutover; no peer coordination required.

## 7. Testing

**Unit (`stock-service`):**
- Accept saga: asserts **no** buyer premium row is created; seller `+premium` row still created.
- Exercise saga: buyer row `TotalGain == (market−strike)×qty − premium`, `SecurityType="option"`; buyer holding `AveragePrice == market`; seller strike row unchanged; `Backward` deletes the buyer row by key (saga-rollback test).
- Exercise saga, market-price lookup fails: exercise still completes, buyer row skipped, basis falls back to strike (no panic, no money impact).
- Expiry cron: buyer `−premium` row created in expiry month, idempotent on re-run; seller none; bank buyer still recorded but exempt at collection.
- `CollectTax`: a bank-owned positive gain is **not** collected (no debit/credit, no `TaxCollection` row); an equivalent client gain **is**.
- Bank-exempt regression: mixed client+bank gains in the same month → only client taxed.

**Integration (`test-app/workflows`):** end-to-end OTC option: accept → exercise (market > strike) → run monthly collection → assert buyer taxed `15% × ((market−strike)×qty − premium)` on the buyer's account, seller taxed `15% × premium`, and an actuary-on-behalf-of-bank variant pays **zero**. A second flow: accept → expiry → collection asserts buyer's month gain reduced by the premium.

**Saga-safety checks:** reuse the fault-injection harness (`contract/shared/saga/faults_*`) to force-fail the buyer-exercise step and assert full compensation (no buyer CG row, shares returned, money restored) — proving the new step body's `Backward` is correct.

## 8. Cross-cutting deliverables (per CLAUDE.md)

- **`Specification.md`:** update business-rules (§21) with the option premium/exercise/expiry tax rules and the bank exemption; note the basis step-up.
- **REST/Swagger:** no new routes expected; if the tax-preview/summary response surfaces option rows distinctly, update `docs/api/REST_API_v3.md` accordingly. (Confirm during implementation.)
- **`VERSION`:** MINOR bump (new backward-compatible tax behaviour; no contract break).
- **Lint:** `make lint` on `stock-service` (and `transaction-service` if touched).
- **Kafka:** no new topics; existing OTC events already publish. (Notification coverage for these events is SP5, not SP1.)

## 9. Risks & mitigations

- **Saga fragility (primary concern):** mitigated by never adding/removing steps — only filling/emptying existing step bodies — and by fault-injection rollback tests. Pre-saga snapshots (market price) fail the exercise *cleanly before any money moves* only where safe; tax-row failures degrade to log+skip and never block settlement.
- **Double taxation:** mitigated by the market-price basis step-up (§3.1), with an equivalence proof.
- **Cross-bank buyer taxation:** deferred as a documented gap (§5) because the frozen SI-TX wire carries no premium and the peer handler has no price source. No money gap; cross-bank sellers still taxed; cross-bank buyers taxed via eventual stock sale as before.
- **Cutover double-count:** mitigated by the one-time idempotent cleanup (§6).
