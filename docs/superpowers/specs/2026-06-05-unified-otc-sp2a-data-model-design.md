# Unified OTC — SP-2a: unified data model (fold remote into local, bank-scoped keys)

**Date:** 2026-06-05
**Status:** Approved (design)
**Parent:** `2026-06-04-unified-otc-local-remote-umbrella-design.md`
**Predecessor:** SP-1 (`2026-06-04-unified-otc-sp1-read-model-design.md`, merged 75e2308) unified the *reads* with the remote data still in separate mirror tables.
**Scope rule:** SP-2a is a **data-model consolidation only — NO client-facing route changes.** Reads and the frozen `/cross-bank-protocol/*` interop behave identically before and after. The write-route unification + dispatch relocation is **SP-2b** (separate spec).

## 1. Goal

Collapse the three separate remote stores (`remote_otc_offer`, `peer_otc_negotiation`, `peer_option_contract`) into the local entity tables (`OTCOffer`, `OTCNegotiation`, the option contract table), so each entity has **one** table holding both local and remote rows. Local vs remote is distinguished by the **bank-scoped natural key**, never a `kind` column; `kind` is derived for the frontend only. This is the foundation SP-2b needs to dispatch writes uniformly.

## 2. Unified identity (decision (ii): surrogate PK + bank-scoped natural key)

Each unified table gains, alongside its existing `ID uint64` surrogate primary key (kept for FK/association stability):

```
RoutingNumber int64  `gorm:"uniqueIndex:ux_<tbl>_native,priority:1;not null"`
NativeID      string `gorm:"uniqueIndex:ux_<tbl>_native,priority:2;size:128;not null"`
```

- **Local row:** `RoutingNumber = OWN_ROUTING`, `NativeID = strconv(ID)` (the surrogate id as the issuing bank — us — knows it).
- **Remote row:** `RoutingNumber = <peer routing>`, `NativeID = <peer's foreign id>` (the offer/negotiation/contract id as the issuing peer bank knows it).
- `(RoutingNumber, NativeID)` is **UNIQUE** and is the authoritative identity. All backend logic locates and distinguishes rows by it; **local-vs-remote is `RoutingNumber == OWN_ROUTING`**.
- **`kind` is derived FE-only** at response-shaping time (`RoutingNumber == own ? "local" : "remote"`). No `kind` column in the DB. (SP-1's proto `kind`/`me_owner`/provenance response fields stay; their values are now computed from `RoutingNumber`.)
- The surrogate `ID` remains the value the frontend addresses in the unified routes (continuity with SP-1's surrogate ids); the natural key is the backend's internal identity.

## 3. Collision prevention (money-safety invariant)

Because `RoutingNumber == own` ⇒ treated as **local** (and entered into local accept/settlement logic), a peer sharing our routing/bank code would let peer-originated rows masquerade as local. Defense-in-depth:

1. **Registration guard (primary, RUNTIME):** peers are added dynamically by an admin, so the check is at runtime on **every** add/update — `POST /api/v3/peer-banks` and the `PeerBankAdminService` create+update path MUST reject a peer whose `routing_number` or `bank_code` equals `OWN_BANK_CODE`/`OWN_ROUTING` → HTTP 400 `validation_error` ("peer bank code/routing must differ from this bank's own").
2. **Ingestion guards (defense-in-depth):** the offer refresher and every inbound `/cross-bank-protocol/*` write handler (CreateNegotiation, UpdateNegotiation, DeleteNegotiation, AcceptNegotiation, the public-offer ingestion) MUST reject/skip any payload whose claimed routing/bank-code equals our own — log at WARN and refuse; never persist it as a (local-looking) row.
3. **Startup assertion:** on boot, if any registered peer has `routing == own`, log at ERROR and disable cross-bank ingestion (do not silently ingest). (A fail-fast boot error is acceptable if simpler.)

## 4. Table fold + retire

For each entity, migrate the remote store's rows into the local table as remote rows, then drop the remote store:

| Retire | Fold into | Remote row mapping |
|---|---|---|
| `remote_otc_offer` (SP-1) | `OTCOffer` | `RoutingNumber=PeerRoutingNumber`, `NativeID=ForeignOfferID`; carry seller/ticker/strike/premium/currency/settlement/status; local-only FK fields (`StockID`, `InitiatorAccountID`, …) left null/zero for remote rows (made nullable). |
| `peer_otc_negotiation` | `OTCNegotiation` | `RoutingNumber`=issuing bank routing, `NativeID=ForeignID`; buyer/seller, parent-offer lot key, status, terms from `OfferJSON`. |
| `peer_option_contract` | option contract table | `RoutingNumber`=negotiation issuing routing, `NativeID`=`NegotiationID`/contract foreign id; buyer/seller, ticker/qty/strike/currency/settlement, `Direction` (CREDIT/DEBIT), status. |

- **Local-only columns on remote rows (as implemented):** remote rows can't satisfy local FKs. Only FK columns that sit in a **unique index** are made nullable (so NULL keeps the index collision-free): `OptionContract.OfferID` → `*uint64` (NULL for remote). FK columns WITHOUT a sole unique index stay their original type with a sentinel for remote rows: `OTCOffer.StockID` stays `uint64` = `0` (no FK constraint, just an index; the routing guards keep remote rows out of every stock-join/money path), and `OTCNegotiation.ParentOfferID` stays `uint64` = `0` with `routing_number` added to `ux_otcneg_chain` so the local one-chain invariant is routing-scoped. This minimizes the pointer ripple while preserving every unique invariant.
- **NO data migration (fresh start — decided 2026-06-05).** Not a production system; we do not preserve existing rows. `AutoMigrate` creates the unified schema with the new columns; the retired tables are simply removed from `AutoMigrate` (and dropped) — **no backfill, no row copying.** Existing dev data is discarded; remote offers repopulate via the next poll, remote negotiations/contracts via fresh cross-bank activity. Going forward the **create/ingest paths populate `(routing_number, native_id)`**: local creation stamps `routing=own` via a `BeforeCreate` hook and leaves `native_id` **NULL** (the surrogate id + `routing==own` already identify a local row; populating `native_id` at insert would race on the unique index since the surrogate id isn't known pre-insert); inbound webhook/refresher ingestion stamps `routing=<peer>` and `native_id=<foreign id>`.
- After the fold, the SP-1 offer refresher upserts/reconciles remote offers as `OTCOffer` rows (remote), and `RemoteOTCOfferRepository` is retired (its upsert/reconcile/get move onto the `OTCOffer` repository, scoped to remote rows).

## 5. Money-path guards (the risk — `routing==own` local-only filters)

Every query/operation that assumes "all rows are local" MUST filter `RoutingNumber == OWN_ROUTING` so a remote row can never enter local money/settlement logic. Required guard sites (verified against the SP-2 surface map):

- `OpenNegotiation`, `CounterNegotiation`, `AcceptNegotiation` (incl. first-accept-wins + the contract-formation saga), `RejectNegotiation`, `CancelNegotiation`, `CancelListing`.
- The cascade-cancel sibling query (`ListOpenByParentOfferForUpdate`).
- The OTC expiry cron (must not expire/settle remote rows via local logic).
- The local offer/negotiation read queries that feed local-only views (`ListOpenForCache` local fetch, `ListByBidder`, `ListByParentOffer`, revisions) — these must remain local-only where they back local-only behavior.
- The contract-formation + local exercise sagas.

**Each guard gets a dedicated test asserting a remote row is NOT selected/mutated by the local path.** A missed guard is the single highest risk in this phase.

## 6. Repoint reads + inbound webhooks + refresher (behavior unchanged)

- SP-1 reads (`GetOffer` remote-resolve, discovery `ListUnifiedOptionOffers`, `ListMyNegotiations`/history/contracts/on-listing/timeline) now query the **one** unified table per entity. The SP-1 two-source merges collapse into a single query (filter by routing for local/remote, derive `kind`) — eliminating the read-time double-query.
- Inbound `/cross-bank-protocol/*` handlers (`CreateNegotiation`, `UpdateNegotiation`, `DeleteNegotiation`, `AcceptNegotiation`, `CascadeCancelSiblings`, public-offer ingestion) write/read the unified tables (remote rows) instead of the retired stores. **The wire is unchanged** — only the persistence target changes.
- `me_owner`/provenance computation now derives `kind`/`routing_number`/`bank_code` from the row's `RoutingNumber` (and `bank_code` from the stored peer code where available) rather than from a separate-table or `kind`-column source.

## 7. Out of scope (SP-2b)

No client-facing route changes; `/me/peer-otc/*` and `POST /me/otc/contracts/peer/:id/exercise` still exist and still work (their handlers now read/write the unified tables). Moving the cross-bank **dispatch** into stock-service and unifying/deleting the **write routes** is SP-2b.

## 8. Testing

- **Identity (fresh start, no migration):** creating a local offer/negotiation/contract stamps `routing=own` + `native_id=strconv(id)` (AfterCreate); inbound webhook ingestion stamps `routing=<peer>` + `native_id=<foreign id>`; the `UNIQUE(routing_number, native_id)` constraint is enforced (duplicate ingest is rejected/idempotent). No backfill of pre-existing rows is tested (there are none — fresh DB).
- **Collision prevention:** registration rejects a peer with `bank_code==own` (400); inbound webhook rejects a payload claiming `routing==own`; refresher skips a peer offer claiming `routing==own`; startup assertion fires when a colliding peer exists.
- **Money-path guards (one per guarded op):** a remote `OTCOffer`/`OTCNegotiation` row is never returned/locked/mutated by `OpenNegotiation`/`Accept`/cascade/`CancelListing`/expiry/local-cache fetch.
- **Reads unchanged:** SP-1 read integration tests still pass against the unified tables (same response shapes incl. `kind`/`me_owner`).
- **Interop unchanged:** inbound webhook create/counter/accept/cancel still produce identical observable behavior (now on the unified tables).
- `make build` / `make lint` / `make test` clean; update `Specification.md` (entities + the collision invariant) and `docs/api/REST_API_v3.md` only where response provenance derivation is described (no route changes).

## 9. Removed / retired in SP-2a

- Tables/models: `remote_otc_offer` (+ `RemoteOTCOfferRepository`), `peer_otc_negotiation` (+ repo), `peer_option_contract` (+ repo) — folded and dropped. Any now-orphaned mapper/helper removed (no stale code). `kind` columns/source-based kind removed in favor of derived kind.
- No routes removed (SP-2b does that).
