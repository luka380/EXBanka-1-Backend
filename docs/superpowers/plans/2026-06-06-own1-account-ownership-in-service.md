# Plan: OWN-1 — move account ownership checks into account-service

Date: 2026-06-06
Status: PLANNED (not started) — needs a focused, dedicated effort; see "Why not now".

## Goal

Per the SERVICE_REVIEW cross-cutting decision (reverses the CLAUDE.md "Resource
Ownership Verification Requirement"): the gateway should NOT check resource
ownership; each owning service checks ownership of its own resources, using
caller identity propagated via gRPC metadata. This plan covers account-service.

## Findings (current state — verified 2026-06-06)

1. **Only `x-changed-by` is propagated.** The gateway sends a single user id
   over gRPC metadata (`contract/changelog/metadata.go` key `x-changed-by`,
   set by `api-gateway/internal/middleware/changed_by.go`, read by
   `changelog.ExtractChangedBy` for AUDIT ATTRIBUTION only). There is NO
   `principal_type` / `principal_id` / `on_behalf_of_client_id` in metadata.
2. **account-service does ZERO authorization today** — it trusts the gateway.
3. **The gateway's ownership model** is `ResolvedIdentity`
   (`api-gateway/internal/middleware/identity.go`) + `checkAccountOwnership`
   (`api-gateway/internal/handler/validation.go:213`): client → `owner_id ==
   principal_id` & not bank-owned; employee (no on-behalf) → resource must be
   bank-owned; employee on-behalf → `owner_id == on_behalf_client_id` + the
   `*.on_behalf_client` permission. 404-vs-403 semantics matter.
4. **Most admin account RPCs have NO per-resource ownership today** — they are
   employee-only routes gated by `RequirePermission` (an employee with the perm
   may act on any account, by design). Per-resource ownership exists mainly on
   the client `/me/*` reads (GetMyAccount/GetMyAccountActivity) and the OTC
   account-binding handlers (`ResolveAndCheckAccount[ByNumber]`).

## The critical split: Tier-A (user-facing) vs Tier-B (service-to-service)

**Tier-B — service-to-service money ops; carry NO caller identity; MUST stay
callable by trusted services with NO client-ownership check:**
`UpdateBalance`, `ReserveIncoming`, `CommitIncoming`, `ReleaseIncoming`,
`ReserveOutgoing`, `SettleOutgoing`, `ReleaseOutgoing`, `ReserveFunds`,
`ReleaseReservation`, `PartialSettleReservation`. Callers: transaction-,
stock-, credit-, interbank-service. **Getting this wrong breaks transfers/OTC/
loans or opens a hole in the money path.**

**Tier-A — user-facing; CAN enforce ownership:** `GetAccount`,
`GetAccountByNumber`, `ListAccountsByClient`, `UpdateAccountName`,
`UpdateAccountLimits`, `UpdateAccountStatus`, `GetLedgerEntries`,
`CreateAccount` (owner_id supplied by caller — validate against identity).

## Design

### Phase D0 — identity-propagation foundation (CROSS-SERVICE, do first)
- New shared package `contract/identity` (or extend `contract/changelog`):
  metadata keys `x-principal-type` (employee|client|**service**),
  `x-principal-id`, `x-on-behalf-client-id`, plus a typed `Caller` struct +
  `Inject(ctx, Caller)` / `FromContext(ctx) (Caller, ok)`.
- A **service principal**: internal service-to-service calls set
  `x-principal-type=service` (a shared client interceptor stamps it for callers
  that are services, not the gateway). Tier-B RPCs require `service` (or accept
  absent identity as service, for backward-compat — decide explicitly).
- Gateway: extend the outbound interceptor to inject the full `Caller` from
  `ResolvedIdentity` (not just changed_by).
- **Backward-compat:** legacy callers send no identity → Tier-B still works
  (treat absent as trusted service), Tier-A fails closed (no identity → 401/403).

### Phase D1 — enforce in account-service (additive; keep gateway checks)
- Read `Caller` in the account handlers; for Tier-A RPCs apply the same
  client/employee/on-behalf rules `checkAccountOwnership` uses today (load the
  account, compare owner_id, 404 vs 403). Tier-B RPCs: require service principal
  (or absent), no ownership check.
- Keep the gateway checks in place during D1 (defense-in-depth; never a window
  where NEITHER checks). Add account-service unit tests for every rule + tier.

### Phase D2 — remove gateway ownership checks for account resources
- Only after D1 is proven: delete `checkAccountOwnership` /
  `ResolveAndCheckAccount[ByNumber]` call sites for account resources from the
  gateway, and the helpers if unused elsewhere. Preserve cross-bank money-path
  guarantees ([[project_crossbank_adversarial_findings]]). Update CLAUDE.md.

## Test plan
- account-service: table tests for each Tier-A RPC × {client-owner-match,
  client-mismatch→404/403, employee-bank-owned, employee-on-behalf+perm,
  on-behalf-without-perm→403}; Tier-B: service principal allowed, client
  principal rejected.
- gateway: identity injection interceptor test; after D2, regression that
  /me/* still 403s on another user's account.
- integration: a client cannot read/modify another client's account end-to-end;
  transfers/OTC/loans (Tier-B) still succeed.

## Why not now (2026-06-06)
This is a cross-service platform change (new metadata contract touched by ALL
services + the gateway), it is security-critical in the money service, and the
Tier-A/Tier-B split must be exactly right. Another agent is concurrently editing
the shared `contract/` (transaction proto/pb) and interbank-service, so changing
the shared identity/metadata contract now risks conflicts. It deserves its own
focused effort with the foundation (D0) landed and verified before any gateway
check is removed (D2). The other account-service review items (A/B/C/F) shipped
independently (VERSION 2.15.4–2.15.6).
