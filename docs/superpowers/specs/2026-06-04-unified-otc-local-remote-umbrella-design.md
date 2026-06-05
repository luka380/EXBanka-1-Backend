# Unified OTC (local + remote) — umbrella design

**Date:** 2026-06-04
**Status:** Approved (design); decomposition approved by user
**Builds on:** the Celina-5 SI-TX cross-bank work (`2026-04-29-celina5-sitx-refactor-design.md`, `2026-05-31-sitx-wire-conformance-design.md`, `2026-06-02-sitx-option-wire-conformance-design.md`) and the Phase-2 local OTC options marketplace (`2026-05-16-otc-options-marketplace.md`).
**Authority for the wire:** `docs/A protocol for bank-to-bank asset exchange.htm` (the SI-TX spec). The cross-bank wire is **frozen** — this work does not change it.

## 1. Motivation

A user-visible bug exposed the problem: an admin on instance2 cannot bid on instance1's OTC option offer. The unified discovery feed (`GET /api/v3/otc/options`) shows that offer on instance2 as `kind:"remote"` with `offer_id:"1"`, but the only bid route the frontend knows — `POST /api/v3/otc/options/:id/bid` — is the **intra-bank** route. It looks up a *local* offer `1`, finds none (offer `1` is a remote mirror of instance1's listing), and returns `404 {"code":"not_found","message":"OTC offer not found"}`.

The root cause is architectural, not a missing route: OTC has **two parallel client-facing route families** for the same functionality —

| Functionality | Local route | Remote route |
|---|---|---|
| Place bid / open chain | `POST /otc/options/:id/bid` | `POST /me/peer-otc/negotiations` |
| List my chains | `GET /me/otc/options/negotiations` | `GET /me/peer-otc/negotiations` |
| Counter | `POST /me/otc/options/:id/negotiations/:nid/counter` | `PUT /me/peer-otc/negotiations/:rid/:id` |
| Accept | `POST /me/otc/options/:id/negotiations/:nid/accept` | `POST /me/peer-otc/negotiations/:rid/:id/accept` |
| Cancel | `DELETE /me/otc/options/:id/negotiations/:nid` | `DELETE /me/peer-otc/negotiations/:rid/:id` |
| Exercise | `POST /otc/contracts/:id/exercise` | `POST /me/otc/contracts/peer/:id/exercise` |

and several local-only capabilities with no remote equivalent (reject, revision history, on-listing negotiations, timeline). The discovery feed already hides the local/remote split behind a `kind` field; the **action** routes never got the same treatment, so the frontend is forced to branch by kind.

## 2. Goal

The frontend must see **one** OTC surface. The backend serves local and remote offers/negotiations through the **same routes**, distinguishing them internally by `kind` and only diverging at the point where the cross-bank protocol is actually invoked. Specifically (user requirements):

1. Keep the **local** route paths as the single client-facing surface; **delete** all `/me/peer-otc/*` routes and `POST /me/otc/contracts/peer/:id/exercise`.
2. No route-level local/remote difference. Any per-kind hint the FE needs is a **body/response field**, never a route.
3. A background **reconciliation poll** of peers: when a peer cancels or finishes an offer/negotiation, our side reflects `cancelled`.
4. Every offer/negotiation GET response carries **`me_owner: true|false`** — does the acting identity own this resource (so the FE parses ownership trivially).
5. **Employees acting for the bank** can bid and counter on remote offers.
6. For a remote offer we surface **only our own client's chain(s)** — never other parties' chains on a listing we don't host. This is expected and acceptable.

## 3. Chosen architecture — converge onto the first-class OTC model (Approach 3)

Remote offers/options become **first-class citizens** alongside local ones, differing only at the cross-bank side-effect boundary. Two facts make this the right and lower-risk choice:

- **The local schema was already designed for cross-bank.** `OTCOffer` (`stock-service/internal/model/otc_offer.go`) already has `InitiatorBankCode *string`, `CounterpartyBankCode *string`, `ExternalCorrelationID *string`, and `ActingEmployeeID *uint64`; `OTCNegotiation` (`stock-service/internal/model/otc_negotiation.go`) already has `BidderBankCode *string`. The cross-bank flow grew a **parallel** mirror (`stock-service/internal/model/peer_otc_negotiation.go`) instead of using these fields. Convergence *finishes* what the schema anticipated and retires the duplicate mirror.
- **The wire treats ids as opaque strings.** `ForeignBankId.id` and the negotiation/offer ids on the SI-TX wire (`contract/sitx/otc_types.go`) have no format constraint — the `^(client|employee)-\d+$` pattern (`api-gateway/internal/handler/peer_otc_handler.go`) is enforced **only** on buyer/seller *principal* ids, never on negotiation/offer ids. The seller's bank mints the negotiation id and peers store/echo it verbatim. So our local surrogate ids (and any composite id we choose to expose) are protocol-legal, and the frozen wire surface does not change.

### 3.1 One record of truth
Remote offers become persisted `OTCOffer` rows (`kind=remote`, a local surrogate `id`, peer routing + foreign id stored in the existing bank-code fields). Our clients' remote negotiations become `OTCNegotiation` rows. The `peer_otc_negotiation` mirror is migrated into these tables and retired. Below the dispatch point, **remote == local**.

### 3.2 Side-effects only at the protocol boundary
Each lifecycle action (bid/counter/accept/reject/cancel/exercise) writes the same rows. When the target is `kind=remote`, the same code path *additionally* fires the existing SI-TX outbound HTTP (the `peer_otc_initiate_handler` proxy logic / the SI-TX NEW_TX dispatch), using the **frozen wire** unchanged. Local targets skip the side-effect.

### 3.3 Bank acts under an employee wire identity; economics stay the bank's
On the SI-TX wire, a bank-side participant is represented as `employee-<N>` (the acting employee — satisfies the frozen principal pattern). Economically it settles against **bank accounts and bank holdings** (owner sentinel `1_000_000_000`), not the employee's personal ones. Symmetric:
- **Bidding**: employee bids → wire id `employee-<N>`, bidder account is a **bank** account.
- **Posting**: when we expose a bank-owned offer on `/cross-bank-protocol/public-option-offers`, we advertise `sellerId = employee-<N>` instead of today's non-conformant `"bank"`/`"0"`. *(This is precisely why the instance1 bank offer is unbiddable today.)*

Interop-safe: peers store the principal id opaquely and resolve display via the existing `GET /cross-bank-protocol/user/:rid/:id`.

### 3.4 `me_owner`
Computed from the acting identity (the existing `ResolveIdentity` / `OwnerIsBankIfEmployee` middleware): for an **employee**, `me_owner=true` when the resource is bank-owned; for a **client**, `me_owner=true` when `owner_id == principal_id`. Added to every OTC offer/negotiation/contract GET response.

### 3.5 Reconciliation
The current offer cache (`stock-service/internal/otccache/`) is **rebuild-from-scratch in memory** — a vanished peer offer simply disappears with no signal, and there is no persisted record. Convergence requires a **persistent** remote mirror, so the poller can **diff**: an offer/negotiation that is gone or terminal on the peer → flip our row to `cancelled` and notify the affected local party. Cadence/patterns reuse the existing otccache refresher and the SI-TX reconciler (`transaction-service/internal/service/peer_tx_reconciler.go`).

## 4. Constraints & non-goals

- **Frozen wire.** The `/api/v3/cross-bank-protocol/*` peer-authenticated routes and the SI-TX JSON shapes (`contract/sitx/`) do NOT change. All unification happens *above* them in the gateway + stock-service.
- **Breaking change, authorized.** Deleting `/me/peer-otc/*` and `POST /me/otc/contracts/peer/:id/exercise` is a breaking change to v3, explicitly authorized by the user (per the API Versioning Compatibility Requirement). No deprecation window — hard cut, FE migrates to the unified routes.
- **No new bank-to-bank capability.** Bidding on a *bank-owned* remote offer is enabled by §3.3 (wire identity), which is in our control; we add no new wire messages.
- **Versioning.** Each implementation phase bumps `VERSION` (MINOR for new unified routes/fields; MAJOR is avoided by deleting routes only with the authorized breaking-change cut — treat the cut as the MAJOR-or-explicitly-authorized step in SP-2). Spec/design docs themselves do not bump VERSION.
- **Docs + tests.** Every route change updates `docs/api/REST_API_v3.md` and Swagger; every change ships unit + integration tests (`test-app/workflows/`). These are restated in each sub-spec.
- **No stale surface (clean cut).** This is a hard requirement, not a follow-up. As each phase supersedes old behavior, the superseded code is **deleted in the same phase** — no dead REST routes, no orphaned gRPC RPCs/handlers/proto messages, no unused service code, no parallel models left behind. Concretely by the end of SP-2/SP-3: the `/me/peer-otc/*` handlers (`peer_otc_initiate_handler.go`) and the `POST /me/otc/contracts/peer/:id/exercise` handler are removed; the `peer_otc_negotiation`/`PeerOtcNegotiation` mirror model + repository + its now-unused `PeerOTCService` RPCs (`RecordOutboundNegotiation`, `ListMyPeerNegotiations`, `MarkNegotiationAccepted`, `UpdateNegotiation`/`DeleteNegotiation` mirror paths, etc. — whichever the converged model no longer calls) are deleted from the proto, the generated `stockpb`, the stock-service handler/service/repo, and the gateway client wiring; Swagger + `docs/api/REST_API_v3.md` + `Specification.md` are pruned to match. Each sub-spec ends with an explicit **"removed/retired"** checklist, and the final phase includes a dead-code sweep (`golangci-lint`'s `unused`, plus a grep for now-dangling identifiers) so nothing is left referencing the retired surface.

## 5. Decomposition (each sub-project: own spec → plan → implementation)

**SP-1 — Unified read model + reconciliation + `me_owner`.** Persist remote offers/negotiations as first-class rows; serve all reads (discovery, my-lists, detail, history, timeline) from the unified model; add `me_owner`; add the peer-cancel reconciliation poll. **Writes still flow through the existing split routes.** Lowest risk; immediately delivers "FE sees one model" for reads. Detailed in `2026-06-04-unified-otc-sp1-read-model-design.md`.

**SP-2 — Unified data model + write routes + dispatch.** Split into two independently-testable phases (decided 2026-06-05):
- **SP-2a — Unified data model.** Fold + retire the three remote stores (`remote_otc_offer`, `peer_otc_negotiation`, `peer_option_contract`) into the local `OTCOffer`/`OTCNegotiation`/contract tables. Identity = surrogate `uint64` PK (FK stability) + `UNIQUE (routing_number, native_id)` bank-scoped natural key; **local-vs-remote is `routing==own`, NOT a `kind` column** (`kind` is derived FE-only). **Collision invariant:** a peer may never have `routing_number`/`bank_code == OWN` — enforced at peer registration + ingestion + startup, so a peer's rows can never masquerade as local. Add `routing==own` money-path guards to every local-only accept/cascade/expiry/settlement query. Repoint SP-1 reads + the inbound `/cross-bank-protocol/*` webhooks to the unified tables. **No client-facing route changes.** Detailed in `2026-06-05-unified-otc-sp2a-data-model-design.md`.
- **SP-2b — Dispatch relocation + write-route unification.** Decision A: move the cross-bank outbound HTTP/HMAC **out of the gateway into stock-service** (gateway shrinks to grpc↔rest + auth/JWT + caching). Unify bid/counter/accept/reject/cancel/exercise on the local routes (stock-service dispatches by `routing==own`); delete `/me/peer-otc/*` and `POST /me/otc/contracts/peer/:id/exercise`. Depends on SP-2a.

**SP-3 — Employee/bank as cross-bank principal.** `employee-<N>` wire identity + bank-account/holding settlement; expose our bank-owned offers conformantly on `/public-option-offers`; allow a bank account as the bidder. Unblocks the original "admin bids on instance1's bank offer" repro end-to-end. Depends on SP-2.

## 6. Risks

- **Surrogate-id stability across cache rebuilds.** Remote offers must keep the same local id between refreshes — keyed on `(routing, foreign_offer_id)` in the persistent mirror (SP-1).
- **Migration of in-flight `peer_otc_negotiation` rows** into the unified model without losing active cross-bank negotiations (SP-2) — needs a careful backfill + a read-bridge during rollout.
- **Reconciliation false-cancels.** A peer that is briefly unreachable must not cause us to cancel live offers — distinguish "peer unreachable" from "offer gone" (SP-1; only diff against a *successful* peer response).
- **Two-stack verification.** Each phase must be verified live against the two hosted instances, matching the team's existing two-stack interop testing practice.
