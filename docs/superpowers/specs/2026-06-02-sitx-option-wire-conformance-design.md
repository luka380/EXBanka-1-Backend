# SI-TX OTC option wire conformance — design

**Date:** 2026-06-02
**Status:** Approved (design)
**Builds on:** `docs/superpowers/specs/2026-05-31-sitx-wire-conformance-design.md` (the conformance pass that landed Tasks 1–19) and the 2026-06-02 two-stack interop verification.
**Authority:** `docs/A protocol for bank-to-bank asset exchange.htm` (the SI-TX spec). Where this doc quotes "the spec," it means that file.

## 1. Motivation

The 2026-06-02 live two-stack interop test exercised the full OTC option lifecycle (negotiate → accept → exercise → counter) and captured every cross-bank message on the wire. It surfaced that the OTC **option** legs are the one part of the SI-TX surface that is **not** byte-conformant to the spec — the conformance pass conformed the envelope, accounts, MONAS legs, postings, votes, and OTC negotiation/discovery, but missed the OPTION asset sub-object and the entire exercise transaction shape. Two concrete gaps:

1. **Accept-side `OptionDescription` is a flat custom dialect.** Spec §2.7.2:
   ```
   type OptionDescription = {
     negotiationId: ForeignBankId,
     stock: StockDescription,        // { ticker }
     pricePerUnit: MonetaryValue,    // { amount, currency }
     settlementDate: ISO8601DateTimeWithTimeZone,
     amount: number,
   }
   ```
   We emit flat `{ ticker, amount, strikePrice, currency, settlementDate, negotiationId, intent }` — i.e. `ticker` instead of nested `stock`, `strikePrice`+`currency` instead of nested `pricePerUnit`, plus a non-spec `intent` field.

2. **Exercise is encoded with our `intent` field, not the spec's pseudo-account form.** Today exercise reuses the OPTION-*asset* marker with `intent:"exercise"` and never emits STOCK legs. The spec expresses exercise entirely differently (see §3.2). A strict cohort peer would reject both.

A non-conformant cross-bank exercise BUG was also found and fixed live this session (the exercise posting builder omitted `AccountType`/`AssetType` tags, so it failed local reserve with `NO_SUCH_ACCOUNT` and never dispatched). That fix is in the working tree and is **superseded** by this design's re-model of the exercise builder.

## 2. How the spec identifies "exercise" (the central question)

The spec does **not** use a flag like our `intent`. It encodes accept vs. exercise in the **shape of the transaction** — whether the option appears as an *asset* or as a pseudo-*account*:

```
type TxAccount =
  | { type: 'PERSON',  id: ForeignBankId }
  | { type: 'ACCOUNT', num: CurrencyAccountNumber }
  | { type: 'OPTION',  id: ForeignBankId }       // the "option pseudo-account"
type Asset =
  | { type: 'MONAS',  asset: MonetaryAsset }
  | { type: 'STOCK',  asset: StockDescription }
  | { type: 'OPTION', asset: OptionDescription } // the option as a tradeable asset
```

- **Accept / forming the contract (§3.6.1):** the option moves as an **asset** — `{type:'OPTION', asset: OptionDescription}` — credited from the seller, debited to the buyer, alongside the premium MONAS legs.
- **Exercise / executing the option (§2.7.2):** no OPTION asset and no `intent` — the option appears as a pseudo-**account** `{type:'OPTION', id: negotiationId}`, and what moves are **MONAS** (strike) and **STOCK** (the underlying).

A receiving bank distinguishes them structurally: **OPTION-as-asset ⇒ accept; OPTION-as-account (with STOCK legs) ⇒ exercise.** This is why removing `intent` is safe *only* together with re-modeling exercise — the transaction shape must carry the operation.

## 3. Design

Chosen approach: **translation-layer (keep internals, change wire encoding + executor recognition).** The internal saga, `peer_option_contracts`, holding reservations, and `RecordOptionContract` settlement are unchanged — they already move money and the underlying stock cross-bank correctly. We change only the `OptionDescription` JSON shape, the exercise posting *builder*, and the receiver *executor*'s recognition of the new leg shapes, routing them to the same internal settlement calls. (Rejected alternative: re-architecting internals into real pseudo-account ledgers — larger, riskier, no functional benefit.)

The `contract/sitx/mapping.go` translation layer already handles OPTION-type accounts (PERSON/ACCOUNT/OPTION) and STOCK assets generically, so the wire plumbing needs no change for the new leg shapes.

### 3.1 Accept-side `OptionDescription` reshape

`contract/sitx/otc_types.go` — change the struct so it marshals to the spec shape:

```
// after (spec §2.7.2)
type OptionDescription struct {
    NegotiationID  ForeignBankId    `json:"negotiationId"`
    Stock          StockDescription `json:"stock"`        // { ticker }
    PricePerUnit   MonetaryValue    `json:"pricePerUnit"` // { amount, currency }
    SettlementDate string           `json:"settlementDate"`
    Amount         int64            `json:"amount"`
}
```

- `Ticker` → nested `Stock` (`StockDescription{ticker}`).
- `StrikePrice` + `Currency` → nested `PricePerUnit` (`MonetaryValue{amount, currency}`, amount a bare JSON number via the existing `DecimalNumber`).
- **`Intent` is deleted from the wire struct.** It becomes an internal-only concept (the builders already know whether they are forming or exercising; the wire no longer needs it).
- Internal producers/consumers that read/write `.Ticker` / `.StrikePrice` / `.Currency` / `.Intent` are updated to the nested fields. Touch-points (from a repo scan): `contract/sitx/{types.go,otc_types.go,mapping.go}`, `transaction-service/internal/sitx/posting_executor.go`, `transaction-service/internal/handler/peer_tx_grpc_handler.go`, `stock-service/internal/handler/peer_otc_grpc_handler.go`, `stock-service/internal/repository/{holding_repository.go,peer_option_contract_repository.go}`.
- `mapping.go` needs no change (it JSON-marshals the struct as-is via `assetToID`/`idToAsset`).

This makes the **accept** NEW_TX option legs byte-conformant.

### 3.2 Exercise re-model to the pseudo-account form

Spec §2.7.2 ("In order to execute an option, an Executing Bank should form a transaction of the form"):

> Debit option pseudo-account for π·k (π = price per unit, k = amount) · Credit the buyer for π·k · Credit option pseudo-account for k stocks · Debit relevant receiving accounts for k assets.
> "An option pseudo-account is a TxAccount of type OPTION whose id is the negotiation ID of the option description whose option shall be executed."

Mapped to our cross-bank wire (buyer at A/111, seller at B/222, `negId` = the contract's negotiationId). Spec sign convention: credit = asset leaves = **negative**; debit = asset arrives = **positive**.

| # | spec leg | wire `account` | wire `asset` | signed amount | processed at |
|---|---|---|---|---|---|
| 1 | debit pseudo π·k | `{type:OPTION, id:{…negId}}` | `{type:MONAS,{currency}}` | **+**π·k | seller bank (B) |
| 2 | credit buyer π·k | `{type:ACCOUNT, num:<buyerAcct>}` | `{type:MONAS,{currency}}` | **−**π·k | buyer bank (A) |
| 3 | credit pseudo k stocks | `{type:OPTION, id:{…negId}}` | `{type:STOCK,{ticker}}` | **−**k | seller bank (B) |
| 4 | debit buyer k stocks | `{type:PERSON, id:{111,client-N}}` | `{type:STOCK,{ticker}}` | **+**k | buyer bank (A) |

Balanced per asset (MONAS +π·k/−π·k = 0; STOCK +k/−k = 0). No OPTION asset, no `intent`.

Internal `Direction` (the executor's inverse word: DEBIT = leaves = spec-negative, CREDIT = arrives = spec-positive):
leg 1 pseudo MONAS → **CREDIT**; leg 2 buyer MONAS → **DEBIT**; leg 3 pseudo STOCK → **DEBIT**; leg 4 buyer STOCK → **CREDIT**.

**Builder** (`stock-service` `InitiateOptionExercise`): replace the 4 OPTION-asset+intent postings with these 4 `SiTxPosting`s, setting `AccountType` (`ACCOUNT`/`PERSON`/`OPTION`) and `AssetType` (`MONAS`/`STOCK`) on every leg (the lesson from the bug fixed live this session). `TxKind` stays `otc-exercise`.

**Receiver executor** (`transaction-service/internal/sitx/posting_executor.go`) — two new recognitions, both routed to the **existing** internal settlement (no saga change):
- **STOCK asset on a PERSON/ACCOUNT leg** (`AssetType=="STOCK"`, `AccountType != "OPTION"`) → resolve the party to their stock holding; credit (leg 4, buyer gains the underlying) at commit.
- **OPTION pseudo-account leg** (`AccountType=="OPTION"`) → look up the local `peer_option_contract` by `negId`; the paired MONAS leg credits the seller's money account, the paired STOCK leg releases + consumes the seller's reserved shares, and the contract is marked used — exactly what `RecordOptionContract`'s exercise branch does today.

### 3.3 Two design decisions made explicit

1. **Pseudo-account ownership = "do I hold the seller side of this contract?", not routing-prefix.** The spec fixes the pseudo-account id to the negotiationId, whose routing is the *negotiation's* bank (111/A here) — but the pseudo-account legs must settle at the bank holding the reserved stock (B). So the executor claims an OPTION-account leg when it holds that negotiationId's contract on the seller side, *regardless* of the id's routing number. The sender's local reserve skips OPTION-account legs (it holds the buyer side). This is the one place the spec is abstract about cross-bank mechanics; our rule is documented here and in code. A bank that finds no local contract for the negotiationId votes NO `OPTION_NEGOTIATION_NOT_FOUND`.

2. **`settlementDate` expiry gate.** Spec §2.7.2: an option "should be unable to execute" once `settlementDate` passes (and reserved resources un-reserve on expiry). The receiver votes NO `OPTION_USED_OR_EXPIRED` when `now > settlementDate` or the contract is already used. The π·k arithmetic on a pseudo-account leg is checked against stored terms; a mismatch votes NO `OPTION_AMOUNT_INCORRECT`. All three reason constants already exist in `contract/sitx/types.go` (`NoVoteReasonOptionUsedOrExpired`, `NoVoteReasonOptionNegotiationNotFound`, `NoVoteReasonOptionAmountIncorrect`) but are not yet emitted; this wires them.

### 3.4 What does NOT change

- Internal saga, reservation lifecycle, `peer_option_contracts` schema, `RecordOptionContract` settlement math, COMMIT/ROLLBACK correlation, reservation-key derivation, the MONAS/account conformance, OTC negotiation/discovery/counter/accept message shapes, and all REST routes. The internal money + stock effects of accept and exercise are identical to today's working behavior; only the wire encoding and the executor's recognition of it change.

## 4. Scope

**In:** `OptionDescription` reshape (nested `stock`/`pricePerUnit`, drop `intent`); exercise builder re-model to MONAS+STOCK+OPTION-pseudo-account; receiver executor recognition of STOCK assets and OPTION-account legs routed to existing settlement; `settlementDate`/used/amount NoVote gates; two new byte fixtures + conformance coverage; unit tests; two-stack re-verify; `Specification.md` §27 update.

**Out:** any internal saga/ledger re-architecture; expiry-sweep cron changes (existing reservation-timeout cron already un-reserves abandoned holds — we only add the *vote-time* expiry gate, not a new sweeper); REST API changes; FX (cross-bank SI-TX has none); the `INSUFFICIENT_ASSETS` prose-vs-type plural discrepancy (we follow the authoritative `type` = `INSUFFICIENT_ASSET`).

## 5. Testing

**Byte fixtures (the cohort gap that let this slip):** add `contract/sitx/testdata/newtx_otc_accept.json` (OPTION-asset NEW_TX with nested `OptionDescription`) and `newtx_otc_exercise.json` (pseudo-account form: OPTION account + STOCK + MONAS legs), wired into `conformance_test.go`. These are the shared byte-targets to hand other teams; the option flows finally get conformance coverage.

**Unit:**
- `contract/sitx`: `OptionDescription` marshals to the spec shape and round-trips; both new fixtures pass `TestConformance`.
- `transaction-service` executor: STOCK-on-PERSON leg credits a holding; OPTION pseudo-account leg routes to seller settlement keyed by negotiationId; `now > settlementDate` → `OPTION_USED_OR_EXPIRED`; unknown contract → `OPTION_NEGOTIATION_NOT_FOUND`; π·k mismatch → `OPTION_AMOUNT_INCORRECT`.
- `stock-service` builder: `InitiateOptionExercise` emits the 4 spec legs with correct account/asset types, signs, and tags.

**Two-stack re-verify (reusing this session's proxy + orchestrator harness pattern):** bring both banks up, run accept → exercise, capture the wire, **diff the captured option legs against the new fixtures**, and confirm money + the underlying stock still move and the contract reaches `exercised` — byte-conformant *and* money-correct.

## 6. Risks & mitigations

- **Touching the exercise money path.** Mitigated by keeping all internal settlement calls identical — only the wire encoding and the executor's *recognition* of it change — and by the two-stack money+stock re-verify before declaring done.
- **Cross-bank pseudo-account routing is spec-abstract.** Mitigated by the explicit ownership-by-contract rule (§3.3.1), documented in code, with `OPTION_NEGOTIATION_NOT_FOUND` as the closed-failure vote when no local contract matches.
- **Hard cutover for the option legs.** Like the original conformance pass, this is a breaking wire change for option flows; it stays local on `Development` until the cohort flag-day. Non-option flows (payments, negotiations) are unaffected.
