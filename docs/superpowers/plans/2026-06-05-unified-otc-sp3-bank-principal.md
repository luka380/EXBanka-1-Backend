# Unified OTC SP-3 (employee/bank cross-bank principal) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use `- [ ]` checkboxes.

**Goal:** Make an employee acting as the bank a first-class cross-bank OTC principal: bank-owned offers publish a conformant `employee-<N>` wire id (not `"bank"`), and the bank can bid/counter/accept/reject/cancel/exercise cross-bank, settling against bank accounts/holdings (sentinel owner `1000000000`). Unblocks the original repro (instance2 admin bids on instance1's bank-owned offer).

**Architecture:** Entirely **stock-service** — the gateway already forwards `acting_employee_id` for a bank-acting employee (`identity.go:83`, every OTC write handler passes `derefU64(identity.ActingEmployeeID)`) and `ResolveAndCheckAccount` already forces a bank-acting employee to bind a bank account. No proto change (every OTC write request already carries `acting_employee_id` / `actor_user_id` + the bank/on-behalf discriminator). The bank's SI-TX party id is **fixed at resource creation** (`ActingEmployeeID` column) and reused for every later wire action on that resource, so a counter by a different employee keeps the same `employee-<N>` the peer expects.

**Tech Stack:** Go, GORM (auto-migrate), gRPC/proto (`make proto` only if a field is touched — none expected), `test-app/workflows`.

**Spec:** `docs/superpowers/specs/2026-06-05-unified-otc-sp3-bank-principal-design.md`. **Branch:** `feature/unified-otc-sp3` (create off Development).

**Key files (from the SP-3 code map):**
- `stock-service/internal/model/otc_offer.go` (InitiatorOwnerType/ID, ValidateOwner), `otc_negotiation.go`.
- `stock-service/internal/handler/peer_otc_grpc_handler.go`: `composePeerSellerID` (~370), `GetPublicOptionOffers` (~283), `parseSellerOwner` (~1546; callers 1207/1289/1502/1622/1654).
- `stock-service/internal/handler/otc_negotiation_remote.go`: `openRemoteNegotiation` (SP-2b guard ~72).
- `stock-service/internal/handler/otc_negotiation_remote_action.go`: `resolveRemoteNegAction` (party auth), counter compose.
- `stock-service/internal/handler/otc_options_handler.go`: `exerciseRemoteContract` (holder auth), `CreateOffer` (~289).

---

## Task 1: `ActingEmployeeID` columns + creation-time capture

**Files:** Modify `stock-service/internal/model/otc_offer.go`, `stock-service/internal/model/otc_negotiation.go`; the offer-create service path (`stock-service/internal/service/otc_offer_service.go` + the handler `CreateOffer`); Test: `stock-service/internal/model/otc_offer_test.go` (or a new `otc_acting_employee_test.go` in handler).

- [ ] **Step 1: Add the columns.** In `OTCOffer` and `OTCNegotiation` add:
```go
// ActingEmployeeID is the employee who ORIGINATED this bank-owned resource.
// Non-nil ONLY when the owner is the bank (InitiatorOwnerType/BidderOwnerType == bank)
// and an employee created it. It is the STABLE SI-TX wire-identity source:
// the resource's bank party publishes as "employee-<ActingEmployeeID>" on every
// wire action, regardless of which employee performs later actions.
ActingEmployeeID *uint64 `gorm:"index"`
```
Place near the existing owner fields. Auto-migrate adds them (no manual migration).

- [ ] **Step 2: Validate the nil-unless-bank rule.** In each model's existing `ValidateOwner`/`BeforeCreate` path, add: if `ActingEmployeeID != nil` then the owner MUST be bank (else return a validation error); the bank owner MAY have it nil (legacy/seed). Do not require it for bank (seed rows are nil).

- [ ] **Step 3: Capture at offer creation.** In the offer-create service (where `InitiatorOwnerType`/`InitiatorOwnerID` are set from `actor_system_type`/`on_behalf_of_client_id`): when the resulting owner is the bank (`on_behalf_of_client_id == 0` and the actor is an employee, i.e. `actor_system_type == "employee"` and `actor_user_id > 0`), set `ActingEmployeeID = &actorUserID`. For a client offer leave it nil.

- [ ] **Step 4: Tests.** A bank offer created by employee 17 → row `ActingEmployeeID == 17`; a client offer → nil; setting `ActingEmployeeID` non-nil on a client-owned row → validation error.

- [ ] **Step 5: Build/test/lint + commit.**
Run: `cd "/Users/lukasavic/Desktop/Faks/Softversko inzenjerstvo/EXBanka-1-Backend/stock-service" && go build ./... && go test ./internal/model/ ./internal/service/ ./internal/handler/ -run "ActingEmployee|Offer|Owner" 2>&1 | tail && golangci-lint run ./internal/... 2>&1 | tail`
Commit: `feat(otc): ActingEmployeeID column + capture at bank-offer creation (SP-3)`

## Task 2: Conformant bank seller id on the wire (`composePeerSellerID` + exposure filter)

**Files:** `stock-service/internal/handler/peer_otc_grpc_handler.go` (`composePeerSellerID` ~370, `GetPublicOptionOffers` ~283); Test: `peer_otc_grpc_handler` test file.

- [ ] **Step 1: Rewrite `composePeerSellerID`.**
```go
func composePeerSellerID(o *model.OTCOffer) string {
    if o.InitiatorOwnerType == model.OwnerBank {
        if o.ActingEmployeeID != nil {
            return "employee-" + strconv.FormatUint(*o.ActingEmployeeID, 10)
        }
        return "" // legacy/seed bank offer w/o acting employee — not exposable cross-bank
    }
    if o.InitiatorOwnerID == nil {
        return ""
    }
    return "client-" + strconv.FormatUint(*o.InitiatorOwnerID, 10)
}
```
- [ ] **Step 2: Filter non-conformant offers from public exposure.** In `GetPublicOptionOffers`, after composing `sellerID`, if it is `""` (a bank offer with no acting employee, or an invalid client row) `continue` (skip the row) and `log.Printf("WARN: offer %d skipped from public exposure: no conformant seller id", o.ID)`. Never publish `"bank"`.
- [ ] **Step 3: Tests.** A bank offer with `ActingEmployeeID=17` → wire `sellerId.id == "employee-17"`; a bank offer with nil acting employee → skipped from `GetPublicOptionOffers` output; a client offer → `client-<id>`; assert `"bank"` never appears in the published list.
- [ ] **Step 4: Build/test/lint + commit.** `feat(otc): publish bank-owned offers as employee-<N> on the SI-TX wire (SP-3)`

## Task 3: Party-id parser handles `employee-<N>` → bank (inbound settlement)

**Files:** `stock-service/internal/handler/peer_otc_grpc_handler.go` (`parseSellerOwner` ~1546 + 6 call sites); Test: same package.

- [ ] **Step 1: Extend the parser** (keep the name `parseSellerOwner` to avoid a wide rename, or rename to `parsePartyOwner` and update all 6 call sites — implementer's choice; if renaming, do it everywhere):
```go
func parseSellerOwner(partyID string) (model.OwnerType, *uint64, error) {
    if partyID == "bank" {
        return model.OwnerBank, nil, nil // back-compat: a peer may still send literal "bank"
    }
    if rest, ok := strings.CutPrefix(partyID, "employee-"); ok {
        if _, err := strconv.ParseUint(rest, 10, 64); err != nil {
            return "", nil, err
        }
        // employee-<N> is WIRE IDENTITY only; local ownership/settlement is the bank.
        // The numeric id is intentionally discarded (kept verbatim in RemoteBuyerID/
        // RemoteSellerID for audit/round-trip).
        return model.OwnerBank, nil, nil
    }
    rest, ok := strings.CutPrefix(partyID, "client-")
    if !ok {
        return "", nil, errors.New("unsupported party id; expected client-<n>, employee-<n>, or bank")
    }
    id, parseErr := strconv.ParseUint(rest, 10, 64)
    if parseErr != nil {
        return "", nil, parseErr
    }
    return model.OwnerClient, &id, nil
}
```
- [ ] **Step 2: Verify the 6 call sites** (1207 share-lock pre-check, 1289 / 1502 reservation, 1622 seller capital-gain, 1654 buyer exercise-credit) all now resolve `employee-<N>` to `(bank, nil)` so a bank seller's shares consume from the bank holding + capital gain attributes to the bank, and a bank buyer's exercised shares credit the bank holding. No logic change at the call sites — they already branch on `(ownerType, ownerID)`.
- [ ] **Step 3: Tests.** `parseSellerOwner("employee-17") == (OwnerBank, nil, nil)`; `("client-9") == (OwnerClient, &9, nil)`; `("bank") == (OwnerBank, nil, nil)`; `("garbage")` → error; `("employee-x")` → error.
- [ ] **Step 4: Build/test/lint + commit.** `feat(otc): parse employee-<N> party id to bank ownership for inbound settlement (SP-3)`

## Task 4: Outbound bid as the bank (lift the SP-2b guard)

**Files:** `stock-service/internal/handler/otc_negotiation_remote.go` (`openRemoteNegotiation`, guard ~72, buyerID ~79, row persist); Test: `otc_negotiation_remote_test.go`.

- [ ] **Step 1: Remove** the `if bidderOwnerType != OwnerClient … FailedPrecondition` guard.
- [ ] **Step 2: Build buyerID by owner.**
```go
var buyerID string
switch bidderOwnerType {
case model.OwnerClient:
    if bidderOwnerID == nil {
        return nil, false, status.Error(codes.InvalidArgument, "client bidder requires an owner id")
    }
    buyerID = "client-" + strconv.FormatUint(*bidderOwnerID, 10)
case model.OwnerBank:
    if actingEmployeeID == 0 { // from req.GetActingEmployeeId()
        return nil, false, status.Error(codes.InvalidArgument, "bank bidder requires an acting employee id")
    }
    buyerID = "employee-" + strconv.FormatUint(actingEmployeeID, 10)
default:
    return nil, false, status.Error(codes.InvalidArgument, "unsupported bidder owner type")
}
```
- [ ] **Step 3: Bidder-account validation for the bank case.** The existing account fetch + validation (owner/active/currency==premium) must assert the account is a BANK account (`is_bank_account` / owner sentinel `1000000000`) when `bidderOwnerType == OwnerBank`, instead of `owner == client principal`. Reuse the same account-service `GetAccount`; branch the ownership assertion on owner type. (The gateway already enforced this, but stock-service re-validates per the SP-2b pattern.)
- [ ] **Step 4: Persist `ActingEmployeeID`** on the new remote `OTCNegotiation` row when `bidderOwnerType == OwnerBank` (`= &actingEmployeeID`), nil for a client bid. Set `RemoteBuyerID = buyerID`, `RemoteBuyerRouting = ownRouting` as today.
- [ ] **Step 5: Tests (fake peer + fake account client).** A bank bid → `buyerId == "employee-<N>"` on the wire, row `ActingEmployeeID` set, bank-account asserted; a bank bid with a non-bank account → error; a bank bid missing acting_employee_id → InvalidArgument; a client bid still works unchanged; wire amounts stay JSON numbers.
- [ ] **Step 6: Build/test/lint + commit.** `feat(otc): cross-bank bid as the bank (employee-<N> buyer id, bank-account settlement) (SP-3)`

## Task 5: Counter/accept/reject/cancel + exercise authorize the bank party

**Files:** `stock-service/internal/handler/otc_negotiation_remote_action.go` (`resolveRemoteNegAction` + counter compose), `stock-service/internal/handler/otc_options_handler.go` (`exerciseRemoteContract`); Tests: the remote-action + exercise test files.

- [ ] **Step 1: `resolveRemoteNegAction` — authorize the bank party.** Today it requires a client principal (`client-<actorId>`) matching `RemoteBuyerID`/`RemoteSellerID` on the side we host. Extend: when the side we host is bank-owned (the matching `Remote*ID` has the `employee-` prefix, equivalently the row's `ActingEmployeeID != nil`), authorize a caller acting as the bank (`callerOwnerType == "bank"` / `acting_principal_type == "bank"`). Keep the client path. A caller who is neither the client party nor (for a bank chain) acting-as-bank → `NotFound` (no leak). Counterparty + `(rid, foreignID)` still from the row.
- [ ] **Step 2: Counter compose reuses the row's wire id.** When composing the counter `OtcOffer`, the bank party's id (whichever side we host) = `"employee-<row.ActingEmployeeID>"` read from the row — NOT recomputed from the acting employee — so a counter by a different employee is wire-stable. The counterparty id is the stored remote id.
- [ ] **Step 3: `exerciseRemoteContract` — authorize the bank buyer.** Today it requires `RemoteDirection == "CREDIT"` && caller `client-<actorId>` == `RemoteBuyerID`. Extend: when `RemoteBuyerID` is `employee-<N>` (bank-hosted buyer) and the caller is acting as the bank (employee actor, `on_behalf_of_client_id == 0`), authorize the exercise. Settlement uses the bank's bound account/holding (`buyer_account_number` already gateway-validated as a bank account for the bank case; the credited holding owner resolves via `parseSellerOwner` → `(bank, nil)`). A non-bank/non-holder → NotFound.
- [ ] **Step 4: Tests.** Remote counter on a bank-hosted chain by a *different* employee → wire `employee-<original N>` (stable), proxied + mirrored. Remote accept/reject/cancel by the bank party → authorized + proxied; by a non-party → NotFound. Remote exercise by the bank buyer → dispatches; by a non-bank caller → NotFound. (Use fake peer/dispatch + fake account.)
- [ ] **Step 5: Build/test/lint + commit.** `feat(otc): authorize the bank party for cross-bank counter/accept/reject/cancel/exercise (SP-3)`

## Task 6: Docs, version, integration, dead-code/guard verify, full build/lint/test

**Files:** `docs/api/REST_API_v3.md`, `docs/Specification.md` (if present), `VERSION` + `api-gateway/internal/version/version.go`, `test-app/workflows/`.

- [ ] **Step 1: Docs.** Document that bank-owned OTC offers are biddable cross-bank (publish `employee-<N>`), and an employee acting as the bank can bid/counter/accept/reject/cancel/exercise cross-bank (settles bank accounts/holdings). Note the wire-identity-stable-per-resource rule. `make swagger` (no route change, but keep generated docs current).
- [ ] **Step 2: Integration (`test-app/workflows`).** Local: a bank-owned OTC offer is created by an employee and appears in the owner's listing with `me_owner=true`; (existing local bank-offer flow still green). Cross-bank bank-principal paths → `t.Skip("requires 2nd stack")` (exercised live in the Docker run). Reuse shared helpers.
- [ ] **Step 3: Guards + money-path.** `cd stock-service && go test ./internal/repository/ -run Guard` green (bank settlement never crosses the routing guard). Dead-code sweep: no leftover `"bank"` literal in wire serialization (`grep -rn '"bank"' stock-service/internal/handler/peer_otc_grpc_handler.go` → only the inbound back-compat parse + non-wire uses).
- [ ] **Step 4: VERSION MINOR bump** (additive columns + lifted guards + conformant wire id; no break) + sync `version.go`.
- [ ] **Step 5: FULL verify (real output, all green).**
```
cd "/Users/lukasavic/Desktop/Faks/Softversko inzenjerstvo/EXBanka-1-Backend"
make build 2>&1 | tail -20
make lint  2>&1 | tail -20
make test  2>&1 | tail -30
```
- [ ] **Step 6: Commit.** `test+docs(otc): SP-3 docs, integration tests, version, full verify`

---

## Self-review notes
- Spec coverage: ActingEmployeeID column + capture (T1); conformant seller-id exposure (T2); inbound employee-<N>→bank parse (T3); outbound bank bid (T4); bank-party authorization for counter/accept/reject/cancel/exercise (T5); docs/version/verify (T6).
- Wire-identity stability: the bank's party id is fixed at creation (`ActingEmployeeID`) and read from the row on every later action (T2 seller exposure, T4 bid persist, T5 counter compose) — a different employee never changes the on-wire id.
- Money safety: settlement always binds bank accounts (gateway `ResolveAndCheckAccount` + T4 stock-service re-validate) / bank holdings (T3 `(bank,nil)` resolution); the routing guards from SP-2a/2b are untouched; no proto/route break.
- No new permission (the unified routes' existing employee gates + bank-account binding are the authorization). No gateway change (identity already forwards acting_employee_id for the bank case).
- The original repro is closed: instance1 publishes its bank offer as `employee-<N>` (T2) → instance2's admin bids as the bank with `employee-<M>` (T4) → instance1 ingests `employee-<M>` → bank settlement (T3) → both sides authorize the bank party through the chain (T5). Validated live in the Docker two-stack run.
