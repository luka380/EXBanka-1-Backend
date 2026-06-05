# Unified OTC — SP-2b: unified write routes + dispatch in stock-service + my-nid on offers

**Date:** 2026-06-05
**Status:** Approved (design) — author-decided under the user's standing "run autonomously, no questions" directive (2026-06-05), consistent with prior answers.
**Parent:** `2026-06-04-unified-otc-local-remote-umbrella-design.md`
**Predecessor:** SP-2a (merged `ead00b1`) folded remote stores into the local tables (bank-scoped key, kind derived, money-path guards). Reads + writes still route through the split surfaces; SP-2b unifies the writes.

## 1. Goal

The frontend uses **one write route per OTC action** regardless of local/remote. The gateway shrinks to grpc↔rest mapping + auth/JWT + caching; **stock-service owns the dispatch** (local saga vs cross-bank HTTP), per the user's decision A ("the gateway should just map grpc↔rest and do auth; it's stock-service's responsibility to deal with offers"). Plus: offer reads gain `my_negotiation_id` so the FE can jump to its own chain. The frozen `/cross-bank-protocol/*` wire is untouched.

## 2. Dispatch relocation (decision A) — cross-bank OTC HTTP moves into stock-service

Today the gateway's `peer_otc_initiate_handler.go` composes the SI-TX `OtcOffer` and makes the authenticated outbound HTTP (X-Api-Key + HMAC) to a peer's `/negotiations` (create/counter/accept/cancel). Exercise already goes gateway→stock-service `InitiateOptionExercise`→transaction-service SI-TX (the target pattern). SP-2b makes negotiation dispatch follow the same shape:

- **New stock-service peer-OTC outbound client** (`stock-service/internal/peerotc/` or similar): resolves a peer via the `PeerBankAdminServiceClient` already wired into stock-service (the otccache refresher uses `ResolvePeerByBankCode`/`ListPeerBanks`), signs with the peer's HMAC outbound key, and performs `POST/PUT/GET/DELETE {peer.base_url}/negotiations[...]`. The HMAC-signing logic is extracted into a small shared helper (so it isn't duplicated between the retiring gateway handler, transaction-service's `sitx` client, and this new client).
- **The gateway's `peer_otc_initiate_handler.go` HTTP/HMAC logic is deleted** and replaced by stock-service gRPC calls. The gateway keeps only request parsing + identity + passthrough.

## 3. Unified write gRPC + routes (dispatch by `routing == own`)

Each write action becomes ONE stock-service gRPC that takes the surrogate id + acting identity + terms, looks up the target row, and dispatches by `routing_number == OwnRouting()`:

| Action | Unified client route (kept) | stock-service dispatch |
|---|---|---|
| Bid / open chain | `POST /api/v3/otc/options/:id/bid` | offer `:id` local → local `OpenNegotiation`; remote → compose SI-TX OtcOffer (buyer = caller, seller from the remote offer row), `POST {peer}/negotiations`, record the remote `OTCNegotiation` row. |
| Counter | `POST /api/v3/me/otc/options/:id/negotiations/:nid/counter` | neg `:nid` local → local `CounterNegotiation`; remote → `PUT {peer}/negotiations/:rid/:id` + mirror update. |
| Accept | `.../:nid/accept` | local → local first-accept-wins saga; remote → `GET {peer}/negotiations/:rid/:id/accept` (begins the peer's option-formation SI-TX) + mirror flip + cascade. |
| Reject | `.../:nid/reject` | local → local reject; remote → `DELETE {peer}/negotiations/:rid/:id` (peer protocol has no reject; cancel is the terminal) + mirror. |
| Cancel chain | `DELETE /api/v3/me/otc/options/:id/negotiations/:nid` | local → local cancel; remote → `DELETE {peer}/negotiations/:rid/:id` + mirror. |
| Exercise | `POST /api/v3/otc/contracts/:id/exercise` | contract `:id` local → local exercise saga; remote → `InitiateOptionExercise` (existing). |

The `:rid`/`:id` (foreign routing + foreign negotiation id) needed for the peer HTTP are read from the remote `OTCNegotiation` row (`routing_number`/`native_id`) — never from the client.

**Deleted (breaking, authorized by the umbrella's clean-cut):** `POST/GET/PUT/POST-accept/DELETE /api/v3/me/peer-otc/negotiations[...]` and `POST /api/v3/me/otc/contracts/peer/:id/exercise`, plus the gateway `PeerOTCInitiateHandler` + the `OTCOptionsHandler.ExercisePeerContract` handler. The `GET /me/peer-otc/negotiations` LIST is already superseded by the unified `GET /me/otc/options/negotiations` (SP-1).

**Ownership/validation** stays gateway-side (the established Resource Ownership requirement): the gateway resolves identity and validates account OWNERSHIP (`ResolveAndCheckAccount`) BEFORE the gRPC call. The currency-matches-premium and active-status checks are performed in STOCK-SERVICE's remote branch, which fetches the bidder account from account-service itself (it needs the account for owner/active/currency validation and to read the account number for `buyerAccountNumber` in the SI-TX wire; no double-fetch from the gateway).

## 4. New feature — `my_negotiation_id` on offer reads

On `GET /api/v3/otc/options` (discovery) and `GET /api/v3/otc/options/:id` (detail), each offer the authenticated caller has an active negotiation chain against (as bidder) carries `my_negotiation_id` (the caller's chain surrogate `nid`) + `my_negotiation_status`, so the FE can link straight to "your bid" without a separate lookup. Computed in the stock-service read layer: for the caller's identity, look up their negotiation chains (`ListByBidder` local + the remote equivalent) keyed by `parent_offer_id`/the remote parent lot key, and stamp the matching offer rows. Absent when the caller has no chain on the offer.

## 5. Proto + clean-up

- Remove the now-unused `ListContractsResponse.peer_contracts`/`peer_total` proto fields (SP-2a left them empty; nothing populates or reads them) — clean-cut. Regenerate `stockpb`.
- New gRPC: extend the OTC write RPCs to carry the acting identity + the bidder/acceptor account so stock-service can dispatch (or reuse the existing request shapes where they already carry it). Add `my_negotiation_id`/`my_negotiation_status` to `UnifiedOptionOffer` + the offer-detail response.
- Delete any gateway client wiring / proto messages that only the retired `/me/peer-otc/*` handlers used.

## 6. Constraints, testing, versioning

- **Frozen wire untouched:** the outbound HTTP that stock-service now makes targets the peer's frozen `/cross-bank-protocol/*` (same payloads as the gateway sent before). The inbound peer-authenticated routes are unchanged.
- **Breaking change (authorized):** deleting `/me/peer-otc/*` + the peer-exercise route is a v3 break the umbrella authorized (FE migrates to the unified routes). VERSION → MAJOR-or-explicit per the umbrella; since this is the authorized breaking cut, bump MINOR is insufficient — treat as the authorized breaking step and bump appropriately (record in the plan; the team runs a single evolving v3, FE is ours).
- **Tests:** unit (stock-service dispatch: local vs remote branch for each action; the new peer-OTC outbound client with a fake peer HTTP server; my-nid stamping) + gateway (passthrough + ownership validation still enforced) + integration (`test-app/workflows`: a unified bid/counter/accept on a local offer; cross-bank ones skip without a 2nd stack; my-nid appears on an offer the caller bid on; the deleted `/me/peer-otc/*` routes now 404).
- **Removed/retired checklist** at the end of the plan; dead-code sweep.

## 7. Out of scope (SP-3)

Employee-as-bank wire identity (`employee-<N>`) + bank-account/holding settlement + exposing our bank-owned offers conformantly. SP-2b makes the routes uniform; SP-3 makes an employee/bank able to actually transact cross-bank end-to-end.
