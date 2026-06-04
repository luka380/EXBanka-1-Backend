# Business Audit Log — design (SP2 / TODO_final item A, Celina 3)

**Date:** 2026-06-04
**Status:** Approved approach (per the SP decomposition) — autonomous build.

## 1. Requirement

A system audit log (Celina 3) recording **who did what, when** for the high-value business actions, viewable by **admins + supervisors** with filters (action type, user, date):

- who changed an employee's **limit** (and to what value)
- who reset an employee's **usedLimit**
- who **approved or rejected** which order
- who changed an employee's **permissions**
- who triggered a **manual tax calculation/collection**

## 2. Existing pattern to reuse

The codebase already has an **admin-cron** audit log built on a clean gateway→Kafka→notification-service→gateway loop:

- Gateway handler publishes `AdminCronActionMessage` (actor `employeeID` taken from JWT) to topic `admin.cron-action` via `api-gateway/internal/kafka/producer.go AuditProducer.PublishCronAction`.
- `notification-service/internal/consumer/admin_audit_consumer.go` consumes it into the `admin_audit_logs` table (`model.AdminAuditLog`).
- `notification-service` exposes `ListAdminAuditLogs` gRPC; gateway `GET /api/v3/admin/audit/cron-actions` (perm `admin.audit.view`) reads it.

**Why mirror this (gateway-publishes) instead of consuming existing domain events:** the domain events (`user.employee-limits-updated`, `stock.order-approved`, `user.role-permissions-changed`, …) **do not carry the actor** ("who"). The gateway always knows the actor from the JWT. Publishing a dedicated audit event from the gateway is the only place all of (actor, action, target, value) are known together, and it exactly matches the proven cron-audit pattern.

## 3. Design

A second audit stream parallel to the cron one:

- **New Kafka topic** `admin.business-action`.
- **New message** `contract/kafka/messages.go: BusinessAuditActionMessage`:
  ```go
  type BusinessAuditActionMessage struct {
      Action          string `json:"action"`            // see enum below
      ActorEmployeeID int64  `json:"actor_employee_id"` // from JWT user_id
      TargetType      string `json:"target_type"`       // "employee" | "order" | "role" | "tax"
      TargetID        string `json:"target_id"`         // stringified id (employee id, order id, role id, "YYYY-MM")
      Detail          string `json:"detail"`            // human-readable: new value / outcome
      Timestamp       int64  `json:"timestamp"`         // unix seconds
  }
  const TopicBusinessAuditAction = "admin.business-action"
  ```
- **Action enum** (string, validated gateway-side): `limit.set`, `limit.used_reset`, `order.approve`, `order.decline`, `permissions.set`, `tax.collect`.
- **New model** `notification-service/internal/model/business_audit_log.go`:
  ```go
  type BusinessAuditLog struct {
      ID         uint64    `gorm:"primaryKey;autoIncrement"`
      Action     string    `gorm:"size:32;not null;index"`
      ActorID    int64     `gorm:"not null;index"` // employee who performed it
      TargetType string    `gorm:"size:32;not null;index"`
      TargetID   string    `gorm:"size:64;not null;index"`
      Detail     string    `gorm:"size:512"`
      Timestamp  time.Time `gorm:"not null;index"`
  }
  ```
- **New consumer** `notification-service/internal/consumer/business_audit_consumer.go` (mirror of `admin_audit_consumer.go`): topic `admin.business-action`, group `notification-service-business-audit`, writes one `business_audit_logs` row per message.
- **New repository** `business_audit_log_repository.go` with `ListAll(filters, page, pageSize)` — filters: `Since`/`Until` (unix), `ActorID`, `Action`, `TargetType` — mirror of `AdminAuditLogRepository`.
- **New gRPC** `ListBusinessAuditLogs` on the notification service (proto + handler), mirroring `ListAdminAuditLogs`.
- **Gateway publish points** — add `AuditProducer.PublishBusinessAction(...)` and call it (best-effort, after the underlying gRPC call succeeds, actor = JWT `user_id`) in:
  - `limit_handler`/employee-limits handler (`PUT /employees/:id/limits`) → `limit.set`, detail = changed fields/values.
  - `actuary_handler.ResetActuaryLimit` (`POST /actuaries/:id/reset-limit`) → `limit.used_reset`.
  - order approve/decline gateway handlers (`POST /orders/:id/approve` / `/decline`) → `order.approve` / `order.decline`, target = order id.
  - permissions handler (set role/employee permissions) → `permissions.set`.
  - `tax_handler.CollectTax` gateway handler (`POST /tax/collect`) → `tax.collect`, target = `YYYY-MM`.
- **Gateway read route** — add `GET /api/v3/admin/audit/business-actions` to the existing `auditAdmin` group (perm `admin.audit.view`), with query filters `?action=&actor_id=&target_type=&since=&until=&page=&page_size=`, returning a paginated list. Handler mirrors `AdminAudit.ListCronActions`.

## 4. Concurrency / safety

- Audit publishes are **best-effort and post-action**: never block or fail the underlying business action if the publish errors (log + continue), matching the cron-audit and in-app-notification conventions.
- Consumer insert is a plain append (no version, no idempotency needed — audit rows are immutable event records; at-least-once delivery may rarely double-log, which is acceptable for an audit trail and matches the cron consumer).
- Topic pre-created via `EnsureTopics(... , TopicBusinessAuditAction)` in **both** notification-service `cmd/main.go` (consumer) and api-gateway producer init (producer), per the Kafka pre-creation requirement.

## 5. Cross-cutting deliverables

- **Proto:** add `ListBusinessAuditLogs` RPC + request/response messages to the notification proto; `make proto`.
- **Swagger + REST docs:** annotate the new gateway route; update `docs/api/REST_API_v3.md`.
- **Specification.md:** new route (§17), new entity (§18), new Kafka topic + message (§19), new enum (§20).
- **docker-compose:** no new env/service (reuses notification-service + its DB); topic auto-created. No compose change required (verify).
- **VERSION:** MINOR bump (new backward-compatible route + topic).
- **Tests:**
  - notification-service unit: consumer writes a row from a `BusinessAuditActionMessage`; repository `ListAll` filters by action/actor/target/date.
  - gateway unit: handler maps query filters → gRPC request; `PublishBusinessAction` invoked on each audited action (table-driven, asserting the produced message via a fake producer); read handler shapes the response.
  - integration (`test-app/workflows`): supervisor changes an employee limit → `GET /admin/audit/business-actions?action=limit.set` shows the entry with the actor and new value; non-admin gets 403.

## 6. Out of scope

- Back-filling historical actions (audit starts recording from deploy).
- Auditing reads (only state-changing business actions are logged).
- The field-level **changelog** tables that already exist for clients/accounts/cards/loans/employees are unchanged; this adds the *action* audit the spec asks for (who/what/when), complementary to those diffs.
