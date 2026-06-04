# Business Audit Log Implementation Plan (SP2)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans. Steps use `- [ ]` checkboxes.

**Goal:** Record who/what/when for limit changes, usedLimit resets, order approve/reject, permission changes, and manual tax collection; expose to admins+supervisors with filters.

**Architecture:** Mirror the existing `admin.cron-action` audit loop: gateway publishes a `BusinessAuditActionMessage` (actor from JWT) → notification-service consumes into `business_audit_logs` → gateway reads via a new `ListBusinessAuditLogs` gRPC under `admin.audit.view`.

**Tech Stack:** Go, GORM, Kafka (segmentio), gRPC/protobuf, Gin.

Reference spec: `docs/superpowers/specs/2026-06-04-business-audit-log-design.md`.

---

## Task 1: Contract — Kafka message + topic

**Files:** Modify `contract/kafka/messages.go`.

- [ ] Add near the other admin messages:
```go
// TopicBusinessAuditAction carries who-did-what audit events published by the
// api-gateway (actor known from JWT) and recorded by notification-service.
const TopicBusinessAuditAction = "admin.business-action"

// BusinessAuditActionMessage is one audited business action.
type BusinessAuditActionMessage struct {
	Action          string `json:"action"`             // limit.set | limit.used_reset | order.approve | order.decline | permissions.set | tax.collect
	ActorEmployeeID int64  `json:"actor_employee_id"`  // JWT user_id of the actor
	TargetType      string `json:"target_type"`        // employee | order | role | tax
	TargetID        string `json:"target_id"`
	Detail          string `json:"detail"`
	Timestamp       int64  `json:"timestamp"`          // unix seconds
}
```
- [ ] `cd contract && go build ./...` → builds.
- [ ] Commit: `feat(contract): BusinessAuditActionMessage + admin.business-action topic`.

## Task 2: Notification proto — ListBusinessAuditLogs RPC

**Files:** Modify `contract/proto/notification/notification.proto`, then `make proto`.

- [ ] Add an RPC mirroring `ListAdminAuditLogs`:
```proto
rpc ListBusinessAuditLogs(ListBusinessAuditLogsRequest) returns (ListBusinessAuditLogsResponse);

message ListBusinessAuditLogsRequest {
  int64 since = 1;        // unix seconds, 0 = no lower bound
  int64 until = 2;
  int64 actor_id = 3;     // employee id, 0 = all
  string action = 4;      // exact match, "" = all
  string target_type = 5; // exact match, "" = all
  int32 page = 6;
  int32 page_size = 7;
}
message BusinessAuditLogEntry {
  uint64 id = 1;
  string action = 2;
  int64 actor_id = 3;
  string target_type = 4;
  string target_id = 5;
  string detail = 6;
  int64 timestamp = 7;
}
message ListBusinessAuditLogsResponse {
  repeated BusinessAuditLogEntry entries = 1;
  int64 total = 2;
  int32 page = 3;
  int32 page_size = 4;
}
```
- [ ] `make proto` → regenerates `contract/notificationpb/*`. `cd contract && go build ./...`.
- [ ] Commit: `feat(proto): ListBusinessAuditLogs RPC on notification service`.

## Task 3: notification-service — model, repo, consumer, handler, wiring

**Files:** Create `model/business_audit_log.go`, `repository/business_audit_log_repository.go`, `consumer/business_audit_consumer.go`; modify `handler/grpc_handler.go`, `cmd/main.go`. Tests: `repository/business_audit_log_repository_test.go`, `consumer/business_audit_consumer_test.go`.

- [ ] **Model** (mirror `admin_audit_log.go`): `BusinessAuditLog{ID, Action(size32,index), ActorID(index), TargetType(size32,index), TargetID(size64,index), Detail(size512), Timestamp(index)}`.
- [ ] **Repository**: `BusinessAuditLogFilters{Since, Until, ActorID, Action, TargetType}` + `ListAll(filters, page, pageSize) ([]model.BusinessAuditLog, int64, error)` ordered by `timestamp DESC`. Mirror `admin_audit_log_repository.go` exactly, adding the `target_type` filter.
  - Test: seed 3 rows, assert filter by action and by actor returns the right subset; pagination total correct. (TDD: write test first, run fail, implement, run pass.)
- [ ] **Consumer** (mirror `admin_audit_consumer.go`): topic `kafkamsg.TopicBusinessAuditAction`, group `notification-service-business-audit`; `handleMessage` unmarshals `BusinessAuditActionMessage` → inserts a `BusinessAuditLog` (`Timestamp: time.Unix(event.Timestamp,0)`).
  - Test: feed a message JSON to `handleMessage`, assert a row is written with the right fields.
- [ ] **gRPC handler** `ListBusinessAuditLogs` (mirror `ListAdminAuditLogs` at grpc_handler.go:256) — map request filters → repo, map rows → `BusinessAuditLogEntry`.
- [ ] **main.go**: `db.AutoMigrate(&model.BusinessAuditLog{})`; add `kafkamsg.TopicBusinessAuditAction` to `EnsureTopics(...)`; construct + `Start` the business audit consumer (mirror admin audit consumer lines); inject the repo into the gRPC handler.
- [ ] `cd notification-service && go build ./... && go test ./...` → pass. Lint.
- [ ] Commit: `feat(notification): business audit log model+consumer+repo+RPC`.

## Task 4: gateway — producer + publish points

**Files:** Modify `api-gateway/internal/kafka/producer.go` (AuditProducer), the producer's `EnsureTopics` init, and the handlers: employee-limits, `actuary_handler.go`, order approve/decline, permissions, `tax_handler.go`. Test: `producer_test.go` (or handler tests with a fake producer).

- [ ] Add `AuditProducer.PublishBusinessAction(ctx, kafkamsg.BusinessAuditActionMessage) error` mirroring `PublishCronAction`.
- [ ] Ensure the gateway's audit producer init includes `kafkamsg.TopicBusinessAuditAction` in its `EnsureTopics`.
- [ ] Add a small gateway helper `auditBusinessAction(c, action, targetType, targetID, detail)` that reads `user_id` from the gin context (JWT) and best-effort publishes (log on error, never fail the request). Place in a shared gateway file (e.g. alongside the cron-audit publish).
- [ ] Call it after a successful underlying gRPC call in each handler:
  - employee-limits set handler → `("limit.set", "employee", id, "<changed fields=values>")`
  - `ResetActuaryLimit` → `("limit.used_reset", "employee", id, "")`
  - order approve handler → `("order.approve", "order", id, "")`
  - order decline handler → `("order.decline", "order", id, "")`
  - permissions set handler → `("permissions.set", "role"|"employee", id, "<perms summary>")`
  - `tax_handler.CollectTax` → `("tax.collect", "tax", "<year>-<month>", "<collected_count>/<total_rsd>")`
- [ ] Test: table-driven — for each handler, a fake `AuditProducer` records the published message; assert action/target/actor. (TDD where the handler is unit-testable; otherwise assert the helper builds the right message.)
- [ ] `cd api-gateway && go build ./... && go test ./...`. Lint.
- [ ] Commit: `feat(gateway): publish business audit actions from limit/order/perm/tax handlers`.

## Task 5: gateway — read route + handler + swagger

**Files:** Modify `api-gateway/internal/router/router_v3.go` (auditAdmin group), `api-gateway/internal/handler/admin_audit_handler.go` (new `ListBusinessActions`), the notification gRPC client wrapper. Regenerate swagger.

- [ ] Add to the `auditAdmin` group: `auditAdmin.GET("/business-actions", h.AdminAudit.ListBusinessActions)`.
- [ ] Handler `ListBusinessActions` (mirror `ListCronActions`): parse `?action=&actor_id=&target_type=&since=&until=&page=&page_size=`, validate `action`/`target_type` enums via `oneOf`, call `notifClient.ListBusinessAuditLogs`, map to JSON, `apiError`/`handleGRPCError` on failure. Full swagger annotations (`@Summary/@Tags/@Param/@Success/@Failure/@Router`).
- [ ] Add the client method to the gateway's notification gRPC client wrapper.
- [ ] `make swagger` (or `cd api-gateway && swag init -g cmd/main.go --output docs`); commit generated docs.
- [ ] `cd api-gateway && go build ./... && go test ./...`. Lint.
- [ ] Commit: `feat(gateway): GET /api/v3/admin/audit/business-actions`.

## Task 6: docs + version

**Files:** `docs/api/REST_API_v3.md`, `docs/Specification.md`, `VERSION`, `api-gateway/internal/version/version.go`.

- [ ] REST doc: add the new endpoint section (auth `admin.audit.view`, query params, example, responses).
- [ ] Specification: §17 route, §18 `BusinessAuditLog` entity, §19 topic+message, §20 action enum.
- [ ] Bump VERSION MINOR (1.1.0 → 1.2.0); sync `version.go`.
- [ ] Commit: `docs(audit): document business audit log; bump VERSION to 1.2.0`.

## Task 7: integration test

**Files:** `test-app/workflows/wf_business_audit_test.go` (`//go:build integration`).

- [ ] Supervisor changes an employee's limit (`PUT /api/v3/employees/:id/limits`) → poll `GET /api/v3/admin/audit/business-actions?action=limit.set` until the entry appears (actor = supervisor id, target = employee id). Assert a non-admin client gets 403 on the audit route.
- [ ] Run against the live stack (`go test -tags integration -run TestWF_BusinessAudit ./workflows/`). Skip-guard on endpoint availability.
- [ ] Commit: `test(audit): integration coverage for business audit log`.

## Self-review
- Spec coverage: limit.set (T4), limit.used_reset (T4), order.approve/decline (T4), permissions.set (T4), tax.collect (T4); read+filters (T3 repo, T5 route); actor-from-JWT (T4 helper). All mapped.
- Type consistency: `BusinessAuditActionMessage` fields (T1) = consumer read (T3) = producer write (T4); proto `BusinessAuditLogEntry` (T2) = handler map (T3) = gateway response (T5).
- Kafka pre-creation: topic added to EnsureTopics in BOTH notification-service (T3) and gateway producer (T4).
