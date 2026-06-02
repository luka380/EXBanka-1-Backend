# SI-TX wire-format conformance — design

**Date:** 2026-05-31
**Status:** Approved (design); plan pending
**Authors:** Claude + lukasavic

## 1. Motivation

Multiple cohort teams interoperate over the bank-to-bank asset-exchange protocol
defined in `docs/A protocol for bank-to-bank asset exchange.htm` (the authors'
SI-TX draft, "the spec"). For interop to work, the **wire format** — the JSON
bodies and HTTP shapes exchanged between banks — must match the spec exactly.

An audit on 2026-05-31 found that our routing, authentication (`X-Api-Key`),
idempotence model, message envelope, vote reason codes, and the 2PC
vote/commit/rollback *flow* all conform — but **every message body is a flatter,
non-conformant dialect**. A peer bank implementing the spec strictly would fail
to interoperate with us on a single call.

This design brings our **wire format** into exact conformance with the spec while
**keeping our internal saga, executor, idempotence/outbound tables, and internal
route names unchanged**. Conformance lives in two layers: the JSON DTOs in
`contract/sitx/*` (rewritten to spec shapes) and the translation functions at the
gateway / transaction-service / stock-service boundaries. The internal gRPC proto
is *enriched* (not reshaped) so no spec data is lost in translation.

### Implementation-grounding correction (2026-05-31)

A second pass that read the *actual* serialization code (not just
`contract/sitx/*`) corrected two OTC findings from the initial audit:

- **OTC negotiation bodies already conform.** The gateway serializes
  `OtcOffer` / `OtcNegotiation` *inline* via `peerOtcOfferReq` and
  `protoOfferToJSON` in `api-gateway/internal/handler/peer_otc_handler.go`, not
  via the `contract/sitx/otc_types.go` structs. The wire shape it emits is
  already spec-shaped: `{stock:{ticker}, settlementDate, pricePerUnit:{…},
  premium:{…}, buyerId, sellerId, amount, lastModifiedBy}` and, on GET,
  `… & {isOngoing}`. **No structural rewrite of the OTC negotiation body is
  needed.** The `contract/sitx/otc_types.go` `OtcOffer`/`OtcNegotiation` structs
  are *internal* (OfferJSON storage + otccache), not the negotiation HTTP wire.
- **The transaction-protocol deviations are real.** `peer_tx_handler.go` genuinely
  builds `sitx.Message[sitx.Transaction]`, `sitx.Posting`, and
  `sitx.TransactionVote` from `contract/sitx/types.go` — those flat structs *are*
  the wire, and §6.1–§6.5 stand.

Remaining OTC gaps after grounding: `/public-stock` shape (§6.8a), `/user` shape
(§6.8b), and monetary `amount` emitted as a JSON string instead of a number
(§6.1a — affects postings *and* OTC money values).

### Decisions locked during brainstorming

- **Cutover:** Hard cut to the spec dialect only. No dual-parsing / no version
  negotiation. Old-dialect peers (including our own older stacks) must upgrade in
  lockstep.
- **Metadata fidelity:** Full — inbound `message` surfaces in the affected user's
  ledger entry; `paymentCode` / `paymentPurpose` / `callNumber` persist on the
  local transaction record and appear in statements.
- **Boundary approach (Approach A):** Spec-shaped wire DTOs + enriched internal
  proto. The saga is *not* rewritten.

## 2. Authoritative reference

The spec is the single source of truth. Section numbers below (e.g. "§2.8.1")
refer to the spec document. Where our implementation uses different *internal*
identifiers, that is irrelevant — only the serialized JSON and HTTP semantics are
judged.

## 3. What already conforms (do not change)

- Transport: JSON over HTTP; `POST {base}/interbank`. Our base URL is
  `…/api/v3/cross-bank-protocol`, registered by peers per §2.9 ("on some base
  URL"). **Keep.**
- Auth: `X-Api-Key` header (§2.10). The optional HMAC headers are a local
  extension layered on top; plain `X-Api-Key` remains accepted. **Keep.**
- `IdempotenceKey { routingNumber, locallyGeneratedKey }` (§2.2). **Keep.**
- `Message<T> { idempotenceKey, messageType, message }` (§2.11). **Keep.**
- `messageType` discriminator values `NEW_TX` / `COMMIT_TX` / `ROLLBACK_TX`
  (§2.12). **Keep.**
- `ForeignBankId { routingNumber, id }` (§2.3). **Keep.**
- NoVote reason code strings (all eight, §2.12.1). **Keep the strings**; only the
  surrounding structure changes (see §6.4).

## 4. Scope rationale (why the out-of-scope items are safe)

The following are deliberately **out of scope** because none of them breaks wire
conformance or interop with a spec-strict peer:

| Item | Why leaving it out is safe |
|---|---|
| **Saga rewrite** | The saga is local execution only. Interop sees only its wire effects (vote / commit / rollback), which become spec-shaped via §6. The spec mandates nothing about local execution shape. Widening scope here adds risk to a component that has broken repeatedly, for zero conformance gain. |
| **Receiver-side 202 async** | §2.11 makes 202 an *option*, not a requirement. Our receiver records the idempotence key and reserves *before* responding 200 — exactly what §2.9 mandates. Residual is operational only: if a synchronous reserve exceeds a peer's timeout, the peer retries and our idempotence cache returns the same vote. Sender-side *handling* of an inbound 202 stays in scope (§7). |
| **`/public-option-offers` + CHECK_STATUS extensions** | Additive endpoints under our base prefix at leaf names the spec doesn't use. No collision. Caveat: cross-bank cascade-cancel grouping (via the non-spec `parentOfferId` field) degrades to no-op with spec-strict peers — a graceful feature degradation, not a conformance break. |
| **Internal route prefix** | §2.9 says `/interbank` sits "on some base URL" that peers register. Our prefix *is* that base URL. Zero impact. |

The genuine risk lives in the **in-scope** changes that brush against saga state:
§6.2 (sign/direction inversion), §6.5 (transactionId correlation), §6.6 (metadata
→ ledger). These are the high-care, heavily-tested sections.

## 5. Architecture

```
Peer bank ──HTTP(JSON spec shapes)──▶ api-gateway
                                        │  (spec DTO  ⟷  internal gRPC proto)
                                        ▼
                              transaction-service / stock-service
                                        │  (enriched proto → existing saga/executor)
                                        ▼
                              account-service, ledger, DB (unchanged shapes)
```

- **`contract/sitx/*.go`** — the JSON DTOs. Rewritten to the exact spec shapes.
  These are the only structs that touch the wire.
- **Gateway handlers** (`peer_tx_handler.go`, `peer_otc_handler.go`,
  `peer_user_handler.go`, …) — translate spec DTO ⟷ internal gRPC proto.
- **Internal gRPC proto** (`contract/proto/transaction/transaction.proto`) —
  *enriched* with the fields needed to carry spec data losslessly (account/asset
  type tags, transactionId as ForeignBankId, transaction metadata).
- **transaction-service / stock-service** — existing saga, executor, repositories
  keep their logic; they consume the enriched proto and persist the new metadata.

## 6. Detailed design

### 6.1 Transaction-protocol DTOs (`contract/sitx/types.go`)

Rewrite to spec shapes. Target JSON (field names = spec):

```go
// §2.6
type TxAccount struct {
    Type string         `json:"type"`           // "PERSON" | "ACCOUNT" | "OPTION"
    ID   *ForeignBankId `json:"id,omitempty"`   // set for PERSON, OPTION
    Num  string         `json:"num,omitempty"`  // set for ACCOUNT
}

// §2.7
type Asset struct {
    Type  string      `json:"type"`  // "MONAS" | "STOCK" | "OPTION"
    Asset interface{} `json:"asset"` // MonetaryAsset | StockDescription | OptionDescription
}
type MonetaryAsset   struct { Currency string `json:"currency"` }     // §2.7.1
type StockDescription struct { Ticker  string `json:"ticker"`  }     // §2.7.3

// §2.8.1 — amount is SIGNED; negative = credit (asset leaves), positive = debit.
// No `direction` field.
type Posting struct {
    Account TxAccount     `json:"account"`
    Amount  DecimalNumber `json:"amount"` // signed; serializes as a JSON number (§6.1a)
    Asset   Asset         `json:"asset"`
}

// §2.8.2
type Transaction struct {
    Postings       []Posting     `json:"postings"`
    TransactionID  ForeignBankId `json:"transactionId"`
    Message        string        `json:"message"`
    CallNumber     string        `json:"callNumber,omitempty"`
    PaymentCode    string        `json:"paymentCode"`
    PaymentPurpose string        `json:"paymentPurpose"`
}

// §2.12.2 / §2.12.3 — transactionId is a ForeignBankId, NOT a string.
type CommitTransaction   struct { TransactionID ForeignBankId `json:"transactionId"` }
type RollbackTransaction struct { TransactionID ForeignBankId `json:"transactionId"` }

// §2.12.1
type TransactionVote struct {
    Vote    string         `json:"vote"`              // "YES" | "NO"
    Reasons []NoVoteReason `json:"reasons,omitempty"` // present only on NO
}
type NoVoteReason struct {
    Reason  string   `json:"reason"`
    Posting *Posting `json:"posting,omitempty"` // the FULL offending posting, not an index
}
```

Reason code constants are unchanged (`UNBALANCED_TX`, `NO_SUCH_ACCOUNT`,
`NO_SUCH_ASSET`, `UNACCEPTABLE_ASSET`, `INSUFFICIENT_ASSET`,
`OPTION_AMOUNT_INCORRECT`, `OPTION_USED_OR_EXPIRED`,
`OPTION_NEGOTIATION_NOT_FOUND`). The `Message[T]` envelope and `IdempotenceKey`
are unchanged.

### 6.1a Monetary amount as a JSON number (cross-cutting)

§2.5 / §2.8.1 are explicit: `amount` "should be represented as a JSON number
production (see ECMA-404)… Do not interpret amount as a float64." Today
`shopspring/decimal` marshals as a **quoted string** by default (no
`MarshalJSONWithoutQuotes` is set anywhere in the repo), so our posting amounts
and OTC `MonetaryValue.amount` currently emit as strings (`"260"`). A spec-strict
peer expecting a bare number (`260`) would mis-parse.

**Approach:** introduce a dedicated wire type that marshals a `decimal.Decimal`
as a *raw JSON number token* (not quoted) and parses a JSON number into a
`decimal.Decimal` without float64 rounding:

```go
// contract/sitx/decimalnum.go
type DecimalNumber struct{ decimal.Decimal }

func (d DecimalNumber) MarshalJSON() ([]byte, error) {
    return []byte(d.Decimal.String()), nil // bare number token, e.g. 260 or 1.5
}
func (d *DecimalNumber) UnmarshalJSON(b []byte) error {
    s := strings.Trim(string(b), `"`) // tolerate quoted input from lenient peers
    v, err := decimal.NewFromString(s)
    if err != nil { return err }
    d.Decimal = v
    return nil
}
```

Use `DecimalNumber` for `Posting.Amount` and `MonetaryValue.Amount` (the wire
DTOs only). Internal storage / proto continue to carry decimal-as-string —
conversion happens in the translation layer. Do **not** set the global
`decimal.MarshalJSONWithoutQuotes` (it would change decimal marshaling across
every service and every unrelated endpoint).

### 6.2 Account/Asset union ↔ internal flat mapping (HIGH CARE)

The internal `SiTxPosting` stays flat (`accountId`, `assetId`, `direction`,
`amount`, `routingNumber`) but the proto gains `account_type` and `asset_type`
tags (§6.7) so the translation is lossless.

**Inbound `Posting` → internal `SiTxPosting`:**

| Spec `account` | internal |
|---|---|
| `{type:"ACCOUNT", num}` | `accountId = num`; `routingNumber` = prefix of `num`; `account_type="ACCOUNT"` |
| `{type:"PERSON", id}` | `accountId = id.id`; `routingNumber = id.routingNumber`; `account_type="PERSON"` (keeps the `client-<n>` participant-resolution path) |
| `{type:"OPTION", id}` | option pseudo-account; `accountId` carries the negotiation `ForeignBankId`; `account_type="OPTION"` |

| Spec `asset` | internal |
|---|---|
| `{type:"MONAS", asset:{currency}}` | `assetId = currency`; `asset_type="MONAS"` |
| `{type:"STOCK", asset:{ticker}}` | `assetId = ticker`; `asset_type="STOCK"` |
| `{type:"OPTION", asset: OptionDescription}` | `assetId` = JSON-encoded internal option terms (built from the spec-shaped `OptionDescription`); `asset_type="OPTION"` |

**Direction by ECONOMIC EFFECT, not by the spec word (the inversion trap):**

The spec's bookkeeping convention is the *inverse* of our internal naming:

- Spec §2.8: a **credit** *reduces* the asset on an account (negative amount —
  the asset leaves). A **debit** *increases* it (positive amount — the asset
  arrives).
- Our executor: internal `CREDIT` → `ReserveIncoming` (asset *arrives*); internal
  `DEBIT` → `ReserveOutgoing` (asset *leaves*).

Therefore the mapping is, by effect:

```
spec amount < 0  (asset leaves account)  → internal DEBIT  (ReserveOutgoing)
spec amount > 0  (asset arrives)         → internal CREDIT (ReserveIncoming)
internal magnitude = abs(amount)
```

**Outbound** is the exact inverse: internal `DEBIT` → negative spec amount,
internal `CREDIT` → positive spec amount; flat `accountId`/`assetId` re-expanded
into the correct `TxAccount` / `Asset` variant using the stored type tags.

**Balance check (§2.8 / `UNBALANCED_TX`):** computed on the **signed** spec
amounts — the sum across all postings, per asset, must be zero. This is done on
the spec representation (before/independently of direction translation) to avoid
sign confusion.

### 6.3 Verification of received transactions (§2.8.6)

Map each spec verification step to a NoVote reason, emitting the **full offending
posting** in `NoVoteReason.posting`:

- Unbalanced → `UNBALANCED_TX` (no posting).
- Account missing → `NO_SUCH_ACCOUNT` (posting).
- Asset missing → `NO_SUCH_ASSET` (posting).
- Account can't hold asset (e.g. stock into currency account, wrong currency) →
  `UNACCEPTABLE_ASSET` (posting).
- Credited (asset-leaving) account lacks funds to reserve → `INSUFFICIENT_ASSET`
  (posting). Option contracts are exempt from the funds check (§2.7.2).
- Option account not credited exactly `k` stocks / debited exactly `k·π` →
  `OPTION_AMOUNT_INCORRECT` (posting).
- Option used or settlement date passed → `OPTION_USED_OR_EXPIRED` (posting).
- Option negotiation id invalid → `OPTION_NEGOTIATION_NOT_FOUND` (posting).

More than one reason may be present. The existing executor already performs these
checks against the flat representation; the translation layer attaches the
spec-shaped offending posting.

### 6.4 Vote response

`TransactionVote` is `{vote:"YES"}` or `{vote:"NO", reasons:[…]}`. The
receiver-generated UUID currently emitted in `SiTxVoteResponse.transaction_id`
is **removed from the wire** (the spec vote carries no id). The UUID may remain
internally for replay-cache bookkeeping but must not appear in the JSON.

### 6.5 transactionId & NEW_TX ↔ COMMIT/ROLLBACK correlation (HIGH CARE)

The spec requires per-message idempotence keys that are **never reused** (§2.9),
and correlates COMMIT/ROLLBACK to the original transaction via `transactionId`
(§2.12.2 / §2.12.3). Today we reuse one idem key across NEW_TX + COMMIT and
correlate on it — a §2.9 violation. The conformant, **surgical** design:

- **Initiator** assigns `transactionId = { routingNumber: ownRouting, id: L }`
  in the NEW_TX `Transaction`, where `L` is the locally generated key it used for
  the **NEW_TX** message. Each message (NEW_TX, COMMIT_TX, ROLLBACK_TX) gets its
  **own** unique `idempotenceKey`.
- **Receiver** stores `transactionId` on its idempotence/tx record at NEW_TX time
  (since `transactionId.id == L`, the NEW_TX record is already keyed under `L`).
  COMMIT/ROLLBACK look up the record by `transactionId` — i.e. by
  `transactionId.id`, which equals the original `L`. Each COMMIT/ROLLBACK
  message's *own* idem key is still recorded for dedup (prevents double-commit on
  retransmit).

Net internal change: the correlation key source moves from
`commit.idempotenceKey.locallyGeneratedKey` → `commit.transactionId.id`. Record
storage and saga step logic are otherwise untouched.

### 6.6 Metadata persistence (§2.8.2; full-fidelity choice)

- `message` → the local ledger entry `description` (via account-service's `memo`
  parameter on the commit-time `UpdateBalance` / credit write).
- `paymentCode`, `paymentPurpose`, `callNumber` → persisted on the inbound TX
  record and surfaced in statements.

Requires: new fields on `SiTxNewTxRequest` (§6.7), new columns on
`peer_idempotence_records` (receiver) and `outbound_peer_txs` (sender), and
threading the values into the commit-path ledger write. The auto-migration
(`AutoMigrate`) handles the column adds.

### 6.7 Internal proto enrichment (`contract/proto/transaction/transaction.proto`)

Additive only (no field removed/renumbered):

- `SiTxPosting`: add `string account_type` (PERSON|ACCOUNT|OPTION) and
  `string asset_type` (MONAS|STOCK|OPTION).
- `SiTxNewTxRequest`: add `SiTxForeignBankId transaction_id`, `string message`,
  `string payment_code`, `string payment_purpose`, `string call_number`. Define
  `SiTxForeignBankId { int64 routing_number = 1; string id = 2; }`.
- `SiTxCommitRequest` / `SiTxRollbackRequest`: `transaction_id` becomes the
  initiator's ForeignBankId. (Migrate the existing `string transaction_id` to the
  new message type, or carry both `routing_number` + `id` — decided in the plan.)
- `SiTxVoteResponse`: stop populating `transaction_id` on the wire response
  (field may remain in proto for internal use but is not serialized to peers).

Run `make proto` after editing.

### 6.8 OTC handlers — what actually needs changing

The OTC *negotiation* wire (`POST/PUT/GET/DELETE/accept /negotiations`) is already
spec-conformant **structurally** (gateway inline serialization, §1.1). The only
negotiation-body change is money amounts:

- **6.8c — OTC money as numbers.** `protoOfferToJSON`
  (`peer_otc_handler.go:411`) emits `pricePerUnit.amount` / `premium.amount` as
  the proto's decimal *string* (`o.GetPricePerStock()`), so they render as JSON
  strings. Wrap them so they render as JSON numbers (§6.1a) — e.g. emit
  `json.RawMessage(decimalString)` or a `DecimalNumber`. Same for the inbound
  `peerMonetaryValueReq.Amount` (today `string`): accept a JSON number. Mirror the
  fix in `peer_otc_initiate_handler.go`'s outbound offer maps (the inline
  `"amount": req.PricePerUnit.Amount` entries) and in `protoToOffer`/`offerToProto`
  on the stock-service side if they round-trip through the wire DTO. Turn-rule,
  409-on-wrong-turn, accept 4-posting formation, and `isOngoing` derivation are
  unchanged and already conformant.

The two structural OTC gaps:

#### 6.8a `GET /public-stock` — bare array grouped by stock (§3.1)

Spec response:

```jsonc
// BARE array, grouped by stock, sellers nested
[ { "stock": {"ticker":"AAPL"}, "sellers": [ {"seller":{"routingNumber":111,"id":"client-3"}, "amount": 50} ] } ]
```

Today (`peer_otc_handler.go:50`) we emit `{"stocks":[{ownerId,ticker,amount,
pricePerStock,currency}]}` — wrapped, flat-per-owner, no `sellers[]`. Changes:

- **Serve side:** in `GetPublicStocks`, group the stock-service rows by `ticker`,
  building `[]{stock:{ticker}, sellers:[{seller: ownerId, amount}]}` and return
  the **bare array** (no `{stocks:…}` wrapper, drop `pricePerStock`/`currency` —
  not in the spec shape). The stock-service proto already returns per-owner rows
  (`PeerPublicStock{owner_id, ticker, amount, …}`); grouping happens gateway-side,
  so **no proto change** is required.
- **Consume side:** `stock-service/internal/otccache/cache.go` `fetchPeer`
  currently unmarshals into `sitx.PublicStocksResponse` (`{stocks:[…]}`). Change
  the consume DTO to the spec bare array `[]PublicStock` with `sellers[]`, and
  flatten each `(stock, seller)` pair into the existing cache `Offer` rows
  (`Kind:"remote"`, `OwnerID`, `Ticker`, `Quantity=seller.amount`). Note the spec
  `/public-stock` carries **no price/currency** — preserve today's behavior of
  leaving remote `PricePerUnit`/`Currency` empty/unknown for discovered stocks
  (the price is negotiated, not advertised).

Rewrite the `contract/sitx/otc_types.go` `PublicStock` / `PublicStocksResponse`
to the spec shape (this struct *is* the consume-side wire DTO):

```go
type StockDescription struct { Ticker string `json:"ticker"` }
type PublicSeller struct {
    Seller ForeignBankId `json:"seller"`
    Amount int64         `json:"amount"`
}
type PublicStock struct {
    Stock   StockDescription `json:"stock"`
    Sellers []PublicSeller   `json:"sellers"`
}
type PublicStocksResponse []PublicStock // BARE array (was struct{ Stocks []… })
```

#### 6.8b `GET /user/{rid}/{id}` — display names (§3.7)

Spec response: `{ "bankDisplayName": string, "displayName": string }`. Today
(`peer_user_handler.go:63,81`) we emit `{id, firstName, lastName}`. Change both
success branches to:

```go
c.JSON(http.StatusOK, gin.H{
    "bankDisplayName": h.ownBankDisplayName,                        // configured bank name
    "displayName":     resp.GetFirstName() + " " + resp.GetLastName(),
})
```

`PeerUserHandler` gains an `ownBankDisplayName string` field (wired from config —
e.g. `OWN_BANK_NAME`, defaulting to the bank code as a string if unset). 404 on
unknown id / foreign rid is unchanged. Update any consumer that reads
`/user` (if otccache or a UI proxy decodes it) to the new shape.

#### Unchanged OTC paths (already conformant — do not touch structurally)

- `POST /negotiations` (§3.2): accepts spec `OtcOffer`, returns `ForeignBankId`.
- `PUT /negotiations/{rid}/{id}` (§3.3): 409 on wrong turn / closed.
- `GET /negotiations/{rid}/{id}` (§3.4): `OtcOffer & {isOngoing}`.
- `DELETE /negotiations/{rid}/{id}` (§3.5).
- `GET /negotiations/{rid}/{id}/accept` (§3.6): 4-posting formation. **Note:** the
  accept postings are composed in `stock-service` (`peer_otc_grpc_handler.go`
  `AcceptNegotiation`) and submitted via `InitiateOutboundTxWithPostings`; when
  §6.1/§6.2 change the internal posting representation, this composition must be
  updated in lockstep to emit the enriched proto postings (account/asset type
  tags, signed-amount semantics).

### 6.9 HTTP semantics (§2.11)

- **Sender** must treat peer responses as: `202` → retry the message later;
  `200` → final, body is the response (e.g. a vote); `204` → final, empty. Verify
  `PeerHTTPClient` handles `202` as retry-later rather than as an error. (Today it
  likely treats non-`200` as failure — confirm and fix.)
- **Receiver** continues to respond `200` (with vote) / `204` (commit/rollback
  ack). Emitting `202` is out of scope (see §4).
- A higher-layer failure (a NO vote) is still `200 OK` with the NO vote in the
  body — never a non-2xx status (§2.11 note).

## 7. Files touched (indicative)

- `contract/sitx/decimalnum.go` — **new** `DecimalNumber` (§6.1a).
- `contract/sitx/types.go` — transaction-protocol DTO rewrite (§6.1).
- `contract/sitx/otc_types.go` — **only** `PublicStock`/`PublicStocksResponse`
  → spec bare-array shape (§6.8a). The `OtcOffer`/`OtcNegotiation` structs are
  internal storage; leave their structure, but switch any money field that
  reaches the wire to `DecimalNumber`.
- `contract/proto/transaction/transaction.proto` (+ regenerated `transactionpb`)
  — enrichment (§6.7). **No stock proto change** for `/public-stock` (grouping is
  gateway-side).
- `api-gateway/internal/handler/peer_tx_handler.go` — full translation rewrite
  (§6.1/§6.2/§6.4).
- `api-gateway/internal/handler/peer_otc_handler.go` — `GetPublicStocks` grouping
  + bare array (§6.8a); `protoOfferToJSON`/`peerMonetaryValueReq` money-as-number
  (§6.8c).
- `api-gateway/internal/handler/peer_user_handler.go` — display-name shape +
  `ownBankDisplayName` field (§6.8b).
- `api-gateway/internal/handler/peer_otc_initiate_handler.go` — outbound offer
  money-as-number (§6.8c).
- `transaction-service/internal/handler/peer_tx_grpc_handler.go`,
  `transaction-service/internal/sitx/posting_executor.go`,
  `transaction-service/internal/sitx/peer_http_client.go` — enriched proto,
  sign/direction mapping, transactionId correlation, 202 handling, metadata.
- `transaction-service/internal/model/peer_idempotence_record.go`,
  `outbound_peer_tx.go` — metadata columns.
- `stock-service/internal/handler/peer_otc_grpc_handler.go` — accept-posting
  composition updated to the enriched internal posting form (§6.8 note).
- `stock-service/internal/otccache/cache.go` — `/public-stock` consume DTO →
  spec bare array (§6.8a).
- `api-gateway` config wiring for `OWN_BANK_NAME`; `docker-compose.yml` +
  `docker-compose-remote.yml` env.
- Specification.md, `docs/api/REST_API_v3.md`, Swagger — doc updates.

## 8. Test plan

**Unit (per service + `contract/sitx`):**

- `DecimalNumber` marshals as a bare JSON number and round-trips (incl. tolerant
  unmarshal of a quoted string); `Posting.Amount` / OTC money render unquoted.
- Golden marshal/unmarshal tests asserting *exact* JSON for every rewritten
  transaction DTO against the spec (`Posting`, `Transaction`, `TransactionVote`,
  `CommitTransaction`, `RollbackTransaction`, the `TxAccount` / `Asset` variants),
  plus the spec `PublicStock[]` bare-array shape and `UserInformation`.
- Union mapping tests including the **sign/direction inversion** (negative →
  internal DEBIT/outgoing, positive → internal CREDIT/incoming) and round-trip
  (inbound → internal → outbound reproduces the original signed posting).
- Balance check on signed amounts (`UNBALANCED_TX`).
- Vote reshaping: NO vote carries `reasons[].posting` as the full posting object.
- transactionId correlation: COMMIT/ROLLBACK resolve the record via
  `transactionId.id`; per-message unique idem keys; retransmit returns cached
  result.
- `GetPublicStocks` groups per-owner rows by ticker into the bare
  `[{stock, sellers[]}]` array; otccache `fetchPeer` decodes that shape into
  cache `Offer` rows.
- `GetUser` returns `{bankDisplayName, displayName}`; 404 on foreign rid unchanged.

**Integration (`test-app/workflows`):**

- Full cross-bank payment NEW_TX → COMMIT asserting spec-shaped wire bodies in
  both directions; balances move correctly (validates the inversion end-to-end).
- NO-vote path asserting `{vote:"NO", reasons:[{reason, posting}]}`.
- OTC negotiate → counter (409 on wrong turn) → accept → exercise asserting spec
  `OtcOffer` / `OptionDescription` and the 4-posting accept transaction.
- `GET /public-stock` (bare array, grouped sellers), `GET /user` (display names),
  `GET /negotiations/{rid}/{id}` (`isOngoing`).
- Metadata: `message` appears on the affected user's ledger entry; `paymentCode`
  / `paymentPurpose` persist and surface in statements.

**Conformance fixtures:** capture the spec's example bodies (e.g. the §2.8 coffee
transaction) as `testdata` and assert our encoder reproduces them
byte-equivalently — the cohort-interop guard.

## 9. Out of scope

Saga rewrite; receiver-side 202 async emission; the `/public-option-offers` and
CHECK_STATUS cohort extensions (kept, clearly marked as local extensions); any
internal route renames. Rationale in §4.

## 10. Risks & mitigations

- **Sign/direction inversion (§6.2):** wrong mapping silently moves money the
  wrong way. Mitigation: map by economic effect with explicit round-trip and
  end-to-end balance-movement tests before any saga code is touched.
- **transactionId correlation (§6.5):** changing the correlation key can strand
  commits/rollbacks. Mitigation: keep `transactionId.id == L` so storage is
  unchanged; only the read-source moves. Test retransmit + dedup.
- **Hard cutover:** all peers (incl. our own older stacks) break until upgraded.
  Mitigation: coordinate the flag-day with the cohort; the conformance fixtures
  give every team a shared byte-level target.
- **Saga fragility:** §6.2 / §6.5 / §6.6 brush against saga state. Mitigation:
  the implementation plan sequences these as isolated, individually-tested steps;
  no unrelated saga refactoring.
