# Notification Coverage Expansion — design + plan (SP5 / TODO_final items D, E, H)

**Date:** 2026-06-04

## Scope (corrected after exploration)
- **D2 card block** and **D3 loan created/approved** already emit in-app + email notifications — verified, no change (documented in the spec).
- **D1 client limit change** — genuine gap: client-service publishes `client.limits-updated` but sends the client no notification; its producer lacks `PublishGeneralNotification`. **Implement.**
- **E OTC contract expiring in N days** — no pre-expiry warning today. **Implement.**
- **H order auto-cancel on settlement-expiry** — stock orders have no `settlement_date` and there is no order-expiry mechanism to fire on; building that is a separate feature, out of notification scope. **Documented as deferred** (OTC offer/contract expiry already notify). 

## D1 — client limit change notification
- Add `PublishGeneralNotification(ctx, GeneralNotificationMessage)` to `client-service/internal/kafka/producer.go` (mirror card-service).
- Add `notification.general` to client-service's `EnsureTopics` call.
- In `ClientLimitService.SetClientLimits` (after the existing `PublishClientLimitsUpdated`): emit an in-app `LIMIT_CHANGED` `GeneralNotificationMessage` to the client (`UserID = client_id`) with `daily_limit`, `monthly_limit`, `transfer_limit`, `currency=RSD`; and a `SendEmail` `LIMIT_CHANGED` to the client's email (looked up via the service's client repo/lookup; skip email gracefully if the email is unavailable). Best-effort — failures log and never block the limit update.
- Templates: add `LIMIT_CHANGED` to `registry_push.go` and `registry_email.go`.

## E — OTC contract expiring-soon warning
- `OptionContractRepository.ListExpiringInNDays(nDays, limit)` → `status='ACTIVE' AND settlement_date = today+nDays` (matches on exactly one calendar day, so the warning fires once per contract).
- In `OTCExpiryCron.RunOnce`, add a warning pass (before/after the expiry pass): for each contract expiring in N days, notify both client parties `OTC_CONTRACT_EXPIRING_SOON` (in-app via the existing `notifyOTCPartyVia`). Data: `ticker`, `settlement_date`, `days_remaining`.
- Config: `OTC_EXPIRY_WARNING_DAYS` (default 3) wired into `NewOTCExpiryCron` via a `WithExpiryWarning(nDays)` builder (0 disables). 
- Template: add `OTC_CONTRACT_EXPIRING_SOON` to `registry_push.go`.
- Scope: intra-bank `option_contracts` only (peer/cross-bank warning deferred — the peer row identifies parties by opaque bank-routed ids, not a local user_id).

## Concurrency / safety
- All notification emits are best-effort, after the underlying action, never blocking it (matches the existing in-app/email convention).
- The expiring-soon pass is naturally idempotent: `settlement_date = today+N` matches a contract on exactly one day, so re-running the cron the same day re-sends at most one duplicate (acceptable for a warning); different days don't match.
- Kafka topic pre-creation: client-service must pre-create `notification.general` (it already pre-creates `notification.send-email`).

## Testing
- **Unit (client-service):** `SetClientLimits` publishes a `LIMIT_CHANGED` general notification (mock producer records it) with the new limits; email attempted when the client email is known.
- **Unit (stock-service):** `ListExpiringInNDays` returns only active contracts dated exactly today+N; the cron warning pass notifies both client parties `OTC_CONTRACT_EXPIRING_SOON` and not bank parties.
- **Unit (notification-service):** new templates render (registry test if present, else covered by the render path).
- **Integration:** a supervisor sets a client's limits → the client's `GET /api/v3/me/notifications` shows a `LIMIT_CHANGED` entry.

## Docs / version
- `Specification.md`: new notification types + the D1/E behavior + the H deferral note. REST doc: none (no new routes). VERSION: MINOR bump.
