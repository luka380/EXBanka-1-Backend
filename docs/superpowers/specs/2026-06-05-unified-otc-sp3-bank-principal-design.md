# Unified OTC — SP-3: employee/bank cross-bank principal

**Date:** 2026-06-05
**Status:** Approved (design) — author-decided under the user's standing "run autonomously, no questions" directive (2026-06-05), consistent with prior answers (employee acts under `employee-<N>` wire identity but settles bank accounts/holdings; sentinel owner `1000000000`; clean-cut; frozen SI-TX wire untouched).
**Parent:** `2026-06-04-unified-otc-local-remote-umbrella-design.md`
**Predecessors:** SP-1 (read model, merged `75e2308`), SP-2a (data-model fold, merged `ead00b1`), SP-2b (unified write routes + dispatch, merged `b64d9cf`).

## 1. Goal

Make an **employee acting as the bank** a first-class cross-bank OTC principal, end-to-end. This unblocks the original repro: instance2's admin (an employee) bidding on instance1's **bank-owned** OTC option offer. Two concrete gaps close:

1. **Bank-owned offers become biddable by peers.** Today a bank-owned offer publishes `seller_id = "bank"` on the SI-TX wire — non-conformant with the frozen `^(client|employee)-\d+$` party-id pattern, so peers can't bid. SP-3 publishes `employee-<N>` instead.
2. **The bank can transact cross-bank.** SP-2b's remote bid/counter/accept/reject/cancel/exercise branches currently *reject* a bank/employee caller with `FailedPrecondition`/`NotFound` (explicit SP-3 deferrals). SP-3 lifts those, building the conformant `employee-<N>` wire identity outbound and accepting it inbound, while **settlement always binds bank accounts/holdings** (sentinel owner `1000000000`, `is_bank_account = true`).

The frozen `/cross-bank-protocol/*` inbound wire and `transaction-service/internal/sitx` are untouched. `employee-<N>` is already a legal wire principal (the frozen pattern admits it), so no peer-facing contract changes.

## 2. The wire-identity invariant (the load-bearing decision)

The bank's SI-TX party id **must be stable within a single offer/chain**, or a counter/accept performed by a *different* employee than the one who opened it would present a *different* `employee-<M>` to the peer, which the peer would reject as "not the same party."

**Decision:** the acting-employee id is **captured once, at the moment the bank resource is created** (offer creation for the seller side; first bid for the buyer side), persisted on the row, and **reused verbatim for every subsequent wire action on that resource**, regardless of which employee performs the later action. So:

- `employee-<N>` where `N` = the employee who *originated* this bank-owned offer (seller side) or *opened* this bank bid chain (buyer side).
- A later counter/accept/cancel/exercise by employee `M ≠ N` still goes on the wire as `employee-<N>` (read from the row).
- Per-resource stability is all the peer needs (it matches party identity per chain, not a global bank identity). Two different bank offers may carry two different `employee-<N>` ids — that is fine.

This means the only new state SP-3 adds is a nullable `ActingEmployeeID` column on the two owner-bearing models, set when the owner is the bank.

## 3. Schema changes (clean-cut, additive columns)

- `OTCOffer`: add `ActingEmployeeID *uint64` (`gorm:"index"`, nullable). Non-nil **only** when `InitiatorOwnerType == bank` and the offer was created by an employee. It is the wire-identity source for a bank-owned offer's `sellerId`.
- `OTCNegotiation`: add `ActingEmployeeID *uint64` (nullable). Non-nil **only** for a bank-owned **bid** chain (the local/remote chain where the bank is the bidder); it is the wire-identity source for the chain's `buyerId`.
- No CHECK constraint beyond "nil unless bank-owner" enforced in the `BeforeCreate`/`ValidateOwner` path. Auto-migrate adds the columns.

These are additive; existing rows get NULL (legacy/seed bank offers — see §6 exposure rule).

## 4. Outbound: building the `employee-<N>` wire identity

### 4.1 Seller-side exposure (`composePeerSellerID`, `GetPublicOptionOffers`)
`stock-service/internal/handler/peer_otc_grpc_handler.go` `composePeerSellerID(o *OTCOffer)`:
- `InitiatorOwnerType == client` → `"client-<InitiatorOwnerID>"` (unchanged).
- `InitiatorOwnerType == bank && o.ActingEmployeeID != nil` → `"employee-<ActingEmployeeID>"`.
- `InitiatorOwnerType == bank && o.ActingEmployeeID == nil` → **filter the offer out of public exposure** (legacy/seed bank offer with no conformant identity; log once). Local visibility is unaffected — only cross-bank publication is suppressed, since publishing `"bank"` is what broke peers. `GetPublicOptionOffers` skips such rows.

### 4.2 Buyer-side bid (`openRemoteNegotiation`)
`stock-service/internal/handler/otc_negotiation_remote.go`:
- **Lift** the SP-2b guard `if bidderOwnerType != OwnerClient … FailedPrecondition`.
- `bidderOwnerType == client` → `buyerId = "client-<bidderOwnerID>"` (unchanged).
- `bidderOwnerType == bank` → require `ActingEmployeeId` on the request (the acting employee from the gateway JWT); `buyerId = "employee-<ActingEmployeeId>"`; persist `ActingEmployeeID` on the new remote `OTCNegotiation` row.
- The bidder account is still validated by stock-service (owner = bank / `is_bank_account`, active, currency == premium) — it fetches the account from account-service exactly as for a client, but the ownership assertion is "bank account" not "client's account."

### 4.3 Counter (`CounterNegotiation` remote branch)
When composing the counter `OtcOffer`, the bank party's id (buyer or seller, whichever side we host) is read from the **row's** `ActingEmployeeID` (→ `employee-<N>`), never recomputed from the acting employee. The counterparty id comes from the stored remote row as always.

## 5. Inbound: accepting `employee-<N>` + settling against the bank

### 5.1 Party-id parsing (`parseSellerOwner` → generalize)
`stock-service/internal/handler/peer_otc_grpc_handler.go`: generalize the party→owner resolver (rename/extend `parseSellerOwner` to a shared `parsePartyOwner(id string)` used for **both** seller and buyer sides):
- `"bank"` → `(OwnerBank, nil)` (kept for backward-compat with any peer still sending it).
- `"client-<n>"` → `(OwnerClient, &n)`.
- `"employee-<n>"` → `(OwnerBank, nil)` — **the employee id is wire identity only; local ownership/settlement is the bank.** The numeric id is not used to look up an employee; it is discarded for settlement purposes (it remains stored verbatim in `RemoteBuyerID`/`RemoteSellerID` for audit/round-trip).
- anything else → error (unchanged).

Every call site (capital-gain attribution on exercise, share lock/consume, buyer-credit on exercise) inherits the fix. Confirm the **buyer-side** owner resolution on exercise-credit (`ExerciseBuyerCreditForPeerOption`) also routes through `parsePartyOwner` so a bank buyer's exercised shares credit the **bank** holding.

### 5.2 Settlement targets
- **Premium** (accept/contract formation): the bank's bound account id (validated at bid/offer time) is used directly by the saga — no party-id parsing needed for the account; it is an explicit account id that is already a bank account.
- **Holdings** (covered-call writer reserve/consume; buyer exercise credit): looked up by `(OwnerType, OwnerID)`. For the bank that is `(OwnerBank, nil)` — the existing holding repository already supports bank-owned holdings; SP-3 only ensures the party-id → `(OwnerBank, nil)` mapping reaches these calls.
- **Capital-gain** records for a bank seller are attributed to `(OwnerBank, nil)` (existing behavior for local bank sellers).

## 6. Authorization (lift the SP-2b bank/employee rejections)

The unified write routes already pass the acting identity (`acting_owner_type` ∈ {client, bank}, `acting_employee_id`) and the gateway already enforces account ownership (`ResolveAndCheckAccount`: an employee with no `on_behalf_of_client` may bind **only** a bank account). SP-3 lifts the stock-service-side rejections so a bank caller is authorized as the chain/contract party:

- **`resolveRemoteNegAction`** (counter/accept/reject/cancel): today it requires a *client* principal matching `RemoteBuyerID`/`RemoteSellerID`. Extend: when the side we host is bank-owned (the row's `ActingEmployeeID != nil` / the wire id is `employee-<N>`), authorize an **employee acting as the bank** (acting_owner_type == bank) as that party. A non-bank/non-matching caller still → `NotFound` (no leak). The `(rid, foreignID)` + counterparty still come from the row, never the client.
- **`exerciseRemoteContract`** (exercise): today it requires caller `client-<id>` == `RemoteBuyerID` on the CREDIT side. Extend: a bank buyer (wire id `employee-<N>`, caller acting as bank) is authorized to exercise the contract it holds. Settlement (premium return / share credit) binds the bank account/holding.

No new permission: the unified routes' existing permission gates (whatever already protects `/otc/options/:id/bid` etc. for employees) suffice; the bank-account binding + these party checks are the authorization.

## 7. Gateway

Minimal/none. `ResolveAndCheckAccount` already validates a bank-acting employee binds a bank account, and the write handlers already forward `acting_owner_type`/`acting_employee_id`. Verify each unified OTC write handler (bid/counter/accept/exercise) forwards `acting_employee_id` for the bank case (bid + exercise especially). If a handler drops it, add the passthrough. No ownership-model change.

## 8. Out of scope

- **Seeding bank holdings/accounts** for the bank to write covered calls or pay premium is a test/operational concern, not SP-3 code — the Docker integration step seeds them. SP-3 only makes the *flows* correct given a funded bank.
- **Cross-bank buyer tax** (a deliberate prior deferral, `project_todo_final_features`) is unchanged.

## 9. Testing

- **Unit (stock-service):** `composePeerSellerID` → `employee-<N>` for a bank offer with an acting employee, filtered when nil, `client-<N>` for a client offer. `parsePartyOwner` maps `employee-<n>`→`(bank,nil)`, `client-<n>`→`(client,&n)`, `bank`→`(bank,nil)`, junk→error. `openRemoteNegotiation` bank branch builds `buyerId = employee-<N>`, persists `ActingEmployeeID`, validates a bank account; a non-bank account → error. Counter reuses the row's `ActingEmployeeID` (stable across a different acting employee). `resolveRemoteNegAction`/`exerciseRemoteContract` authorize the bank party and reject a non-party. Wire amounts stay JSON numbers.
- **Integration (`test-app/workflows`):** local bank-owned offer creation still works; the cross-bank bank-principal paths `t.Skip("requires 2nd stack")` (exercised live in the Docker step). The original repro (employee bids on a bank-owned offer) is validated in the two-stack Docker run.
- **Money-path guards** unchanged and still green; bank settlement never crosses the routing guard.

## 10. Versioning

Additive (new columns + lifted guards + conformant wire id); no route/contract break. `VERSION` MINOR bump. If a proto field must be added to carry `acting_employee_id` where it isn't already present, that is additive (free tag).
