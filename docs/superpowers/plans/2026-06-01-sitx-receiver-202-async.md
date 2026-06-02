# SI-TX Receiver-Side 202 Async Emission — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the receiver return HTTP 202 when a NEW_TX reserve exceeds a configurable deadline (finishing the reserve in the background, cached for the sender's retransmit), instead of holding the HTTP connection until the peer times out.

**Architecture:** Timeout-based. `HandleNewTx` starts the reserve in a background worker and races it against `SITX_RECEIVE_SYNC_DEADLINE` (default 5s). Finishes in time → 200 + vote (fast path unchanged). Exceeds deadline → 202 + a `pending` idempotence row; the worker upgrades the row to `done` when it finishes; the sender's retransmit returns the cached vote. Restart-safe via re-kick-on-retransmit (the reserve is idempotent); no new cron.

**Tech Stack:** Go, gRPC/protobuf, GORM/Postgres, `shopspring/decimal`. Tests: `go test` with the existing transaction-service repo/handler test harness.

**Design reference:** `docs/superpowers/specs/2026-06-01-sitx-receiver-202-async-design.md` (§ tags below point into it).

**Ground rules:** TDD; `make lint` on touched services; `make proto` + commit regenerated files after proto edits; commit on `Development`; never push.

---

## File structure

- `transaction-service/internal/model/peer_idempotence_record.go` — add `Status` column.
- `transaction-service/internal/repository/peer_idempotence_repository.go` — add `UpsertDone` / `UpsertPending`.
- `contract/proto/transaction/transaction.proto` (+ regen) — `SiTxVoteResponse.pending`.
- `transaction-service/internal/config/config.go` — `InterbankReceiveSyncDeadline`.
- `transaction-service/internal/handler/peer_tx_grpc_handler.go` — in-flight set, `runReserve`, `startWorker`, the deadline race in `HandleNewTx`, `cacheAndReturn`→`UpsertDone`.
- `transaction-service/cmd/main.go` — pass the deadline into the handler constructor.
- `api-gateway/internal/handler/peer_tx_handler.go` — map `pending` → 202.
- `docs/api/REST_API_v3.md`, `docker-compose.yml` — docs/config.

---

## Task 1: Record `Status` + idempotent upserts

**Files:**
- Modify: `transaction-service/internal/model/peer_idempotence_record.go`
- Modify: `transaction-service/internal/repository/peer_idempotence_repository.go`
- Test: `transaction-service/internal/repository/peer_idempotence_repository_test.go`

- [ ] **Step 1: Add the `Status` field to the model.** In `PeerIdempotenceRecord`, add after `RolledBackAt`:

```go
	// Status is "pending" while a 202-async reserve runs in the background,
	// or "done" once the vote is cached. Synchronous (fast-path) inserts go
	// straight to "done". Existing rows default to "done".
	Status string `gorm:"size:16;not null;default:'done'"`
```

- [ ] **Step 2: Write the failing repo test.** Append to `peer_idempotence_repository_test.go` (follow the existing test's DB setup — it already constructs a `*gorm.DB` + `NewPeerIdempotenceRepository`):

```go
func TestUpsertPending_ThenUpsertDone(t *testing.T) {
	repo := newTestIdemRepo(t) // use whatever helper/inline setup the existing tests use
	peer, idem := "222", "k-async-1"

	// 1. UpsertPending creates a pending row.
	if err := repo.UpsertPending(&model.PeerIdempotenceRecord{PeerBankCode: peer, LocallyGeneratedKey: idem, TxForeignID: "tx-1"}); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	rec, found, _ := repo.Lookup(peer, idem)
	if !found || rec.Status != "pending" {
		t.Fatalf("want pending row, got found=%v status=%q", found, rec.GetStatusOrEmpty())
	}

	// 2. UpsertDone overwrites pending → done with the vote.
	if err := repo.UpsertDone(&model.PeerIdempotenceRecord{PeerBankCode: peer, LocallyGeneratedKey: idem, TransactionID: "rx-uuid", ResponsePayloadJSON: `{"type":"YES"}`, TxForeignID: "tx-1"}); err != nil {
		t.Fatalf("UpsertDone: %v", err)
	}
	rec, _, _ = repo.Lookup(peer, idem)
	if rec.Status != "done" || rec.ResponsePayloadJSON != `{"type":"YES"}` {
		t.Fatalf("want done+vote, got status=%q payload=%q", rec.Status, rec.ResponsePayloadJSON)
	}

	// 3. UpsertPending against an existing done row is a no-op (does NOT clobber).
	if err := repo.UpsertPending(&model.PeerIdempotenceRecord{PeerBankCode: peer, LocallyGeneratedKey: idem, TxForeignID: "tx-1"}); err != nil {
		t.Fatalf("UpsertPending(2): %v", err)
	}
	rec, _, _ = repo.Lookup(peer, idem)
	if rec.Status != "done" || rec.ResponsePayloadJSON != `{"type":"YES"}` {
		t.Fatalf("UpsertPending clobbered a done row: status=%q payload=%q", rec.Status, rec.ResponsePayloadJSON)
	}
}
```

(Remove the `GetStatusOrEmpty()` call — it's pseudocode; just read `rec.Status` after the `found` check. Use the existing tests' DB-setup helper instead of `newTestIdemRepo` if a different name exists.)

- [ ] **Step 3: Run it, verify it fails.** `cd transaction-service && go test ./internal/repository/ -run TestUpsertPending -v` → FAIL (UpsertPending/UpsertDone undefined).

- [ ] **Step 4: Implement the two upserts.** Add to `peer_idempotence_repository.go` (ensure `"gorm.io/gorm/clause"` is imported):

```go
// UpsertDone writes (or overwrites) the record as status="done" with the
// cached vote + debits/options/meta. Replaces the plain Insert on the cache
// path; on the 202-async path it overwrites the pending row left by the
// timeout. Keyed on (peer_bank_code, locally_generated_key).
func (r *PeerIdempotenceRepository) UpsertDone(rec *model.PeerIdempotenceRecord) error {
	rec.Status = "done"
	if rec.DebitsJSON == "" {
		rec.DebitsJSON = "[]"
	}
	if rec.OptionsJSON == "" {
		rec.OptionsJSON = "[]"
	}
	return r.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "peer_bank_code"}, {Name: "locally_generated_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"status", "transaction_id", "response_payload_json", "debits_json",
			"options_json", "message", "payment_code", "payment_purpose",
			"call_number", "tx_routing_number", "tx_foreign_id",
		}),
	}).Create(rec).Error
}

// UpsertPending creates a status="pending" placeholder row iff none exists
// (ON CONFLICT DO NOTHING) — it never clobbers a done row written by a worker
// that finished as the deadline fired. ResponsePayloadJSON gets a "{}"
// placeholder to satisfy NOT NULL.
func (r *PeerIdempotenceRepository) UpsertPending(rec *model.PeerIdempotenceRecord) error {
	rec.Status = "pending"
	if rec.ResponsePayloadJSON == "" {
		rec.ResponsePayloadJSON = "{}"
	}
	if rec.DebitsJSON == "" {
		rec.DebitsJSON = "[]"
	}
	if rec.OptionsJSON == "" {
		rec.OptionsJSON = "[]"
	}
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "peer_bank_code"}, {Name: "locally_generated_key"}},
		DoNothing: true,
	}).Create(rec).Error
}
```

- [ ] **Step 5: Run it, verify it passes.** `cd transaction-service && go test ./internal/repository/ -run TestUpsertPending -v` → PASS. Then full package: `go test ./internal/repository/`.

- [ ] **Step 6: Lint + commit.**

```bash
cd transaction-service && golangci-lint run ./internal/repository/ ./internal/model/ && cd ..
git add transaction-service/internal/model/peer_idempotence_record.go transaction-service/internal/repository/peer_idempotence_repository.go transaction-service/internal/repository/peer_idempotence_repository_test.go
git commit -m "feat(sitx): idempotence record Status + UpsertDone/UpsertPending"
```

---

## Task 2: Proto `pending` flag

**Files:**
- Modify: `contract/proto/transaction/transaction.proto`
- Regenerate: `contract/transactionpb/`

- [ ] **Step 1: Add the field.** In `SiTxVoteResponse`, append a field (next number after the existing ones — it currently has `type=1`, `no_votes=2`, `transaction_id=3`, so use `4`):

```proto
message SiTxVoteResponse {
  string type = 1;
  repeated SiTxNoVote no_votes = 2;
  string transaction_id = 3;
  bool pending = 4; // receiver still processing (HTTP 202); type/no_votes unset
}
```

- [ ] **Step 2: Regenerate.** `make proto` → confirm `contract/transactionpb/transaction.pb.go` has `SiTxVoteResponse.GetPending()`.

- [ ] **Step 3: Verify contract compiles.** `cd contract && go build ./...` → success.

- [ ] **Step 4: Commit.**

```bash
git add contract/proto/transaction/transaction.proto contract/transactionpb/
git commit -m "feat(sitx): SiTxVoteResponse.pending flag for 202 async"
```

---

## Task 3: Deadline config + background worker + race in HandleNewTx

**Files:**
- Modify: `transaction-service/internal/config/config.go`
- Modify: `transaction-service/internal/handler/peer_tx_grpc_handler.go`
- Modify: `transaction-service/cmd/main.go`
- Test: `transaction-service/internal/handler/peer_tx_grpc_handler_test.go`

- [ ] **Step 1: Add config.** In `config.go` add to `Config`: `InterbankReceiveSyncDeadline time.Duration`. In `Load()`, set it via the existing helper: `InterbankReceiveSyncDeadline: getDuration("SITX_RECEIVE_SYNC_DEADLINE", 5*time.Second),` (match the existing `getDuration(key, fallback)` usage for the other Interbank* timeouts).

- [ ] **Step 2: Extend the handler struct + constructor.** In `peer_tx_grpc_handler.go`:
  - Add imports if missing: `"sync"`, `"time"`, `"context"`.
  - Add fields to `PeerTxGRPCHandler`:
    ```go
    receiveSyncDeadline time.Duration
    mu                  sync.Mutex
    inflight            map[string]chan struct{} // peer:idem -> done signal (closed on worker finish)
    ```
  - Add `receiveSyncDeadline time.Duration` as the LAST param of `NewPeerTxGRPCHandler`, and in the returned struct set `receiveSyncDeadline: receiveSyncDeadline,` and `inflight: make(map[string]chan struct{}),`.

- [ ] **Step 3: Wire the deadline in `cmd/main.go`.** Find the `NewPeerTxGRPCHandler(...)` call and append `cfg.InterbankReceiveSyncDeadline` as the new last argument. (If the handler is constructed in a test-only path too, update those call sites — search for `NewPeerTxGRPCHandler(`.)

- [ ] **Step 4: Refactor the reserve into `runReserve` + add `startWorker`.** In `peer_tx_grpc_handler.go`, add:

```go
// runReserve performs the cheap balance check + reservation and caches the
// result as a done idempotence record. Returns the proto vote. Safe to call
// from a background goroutine (uses the passed ctx) and idempotent on
// (peerCode, idem) — re-running after a crash re-reserves harmlessly because
// account-service reservations are keyed by peer:idem.
func (h *PeerTxGRPCHandler) runReserve(ctx context.Context, peerCode, idem string, postings []contractsitx.InternalPosting, meta txMeta) *transactionpb.SiTxVoteResponse {
	if vote := sitx.BuildPrelimVote(postings); vote.Type == contractsitx.VoteNo {
		resp := voteToProto(vote)
		_, _ = cacheAndReturn(h.idemRepo, peerCode, idem, "", nil, nil, meta, resp)
		return resp
	}
	res := h.executor.Reserve(ctx, postings, peerCode, idem)
	if res.Vote.Type == contractsitx.VoteNo {
		resp := voteToProto(res.Vote)
		_, _ = cacheAndReturn(h.idemRepo, peerCode, idem, "", nil, nil, meta, resp)
		return resp
	}
	txID := uuid.NewString()
	resp := &transactionpb.SiTxVoteResponse{Type: contractsitx.VoteYes, TransactionId: txID}
	_, _ = cacheAndReturn(h.idemRepo, peerCode, idem, txID, res.DebitedItems, res.OptionItems, meta, resp)
	return resp
}

// startWorker ensures a single background worker is running for (peerCode, idem)
// and returns a signal channel that is CLOSED when the worker finishes (after
// the done record is committed). Concurrent callers for the same key share one
// worker and one signal.
func (h *PeerTxGRPCHandler) startWorker(peerCode, idem string, postings []contractsitx.InternalPosting, meta txMeta) chan struct{} {
	key := peerCode + ":" + idem
	h.mu.Lock()
	if sig, ok := h.inflight[key]; ok {
		h.mu.Unlock()
		return sig
	}
	sig := make(chan struct{})
	h.inflight[key] = sig
	h.mu.Unlock()
	go func() {
		// Background context: the worker must outlive the gRPC request that
		// returned 202. (A process restart abandons it; the next retransmit
		// re-kicks — runReserve is idempotent.)
		h.runReserve(context.Background(), peerCode, idem, postings, meta)
		h.mu.Lock()
		delete(h.inflight, key)
		h.mu.Unlock()
		close(sig)
	}()
	return sig
}
```

- [ ] **Step 5: Rewrite `HandleNewTx`'s execute section to race the worker.** Replace the body from after the validation down through the reserve. The new shape:

```go
func (h *PeerTxGRPCHandler) HandleNewTx(ctx context.Context, req *transactionpb.SiTxNewTxRequest) (*transactionpb.SiTxVoteResponse, error) {
	idem := req.GetIdempotenceKey().GetLocallyGeneratedKey()
	peerCode := req.GetPeerBankCode()
	if idem == "" || peerCode == "" {
		return nil, status.Error(codes.InvalidArgument, "missing idempotence_key or peer_bank_code")
	}

	existing, found, err := h.idemRepo.Lookup(peerCode, idem)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "idem lookup: %v", err)
	}
	if found && existing.Status == "done" {
		var cached transactionpb.SiTxVoteResponse
		if json.Unmarshal([]byte(existing.ResponsePayloadJSON), &cached) == nil {
			return &cached, nil
		}
		// corrupt payload — fall through to re-execute
	}

	postings := protoToPostings(req.GetPostings())
	meta := txMeta{
		Message:         req.GetMessage(),
		PaymentCode:     req.GetPaymentCode(),
		PaymentPurpose:  req.GetPaymentPurpose(),
		CallNumber:      req.GetCallNumber(),
		TxRoutingNumber: req.GetTransactionId().GetRoutingNumber(),
		TxForeignID:     req.GetTransactionId().GetId(),
	}

	// A pending row means a worker is (or was) running; re-kick if the process
	// restarted (startWorker is a no-op if already in-flight) and report 202.
	if found && existing.Status == "pending" {
		h.startWorker(peerCode, idem, postings, meta)
		return &transactionpb.SiTxVoteResponse{Pending: true}, nil
	}

	// First delivery: start the worker, race it against the sync deadline.
	sig := h.startWorker(peerCode, idem, postings, meta)
	select {
	case <-sig:
		// Worker finished within the deadline — return the cached vote (200).
		if rec, ok, lerr := h.idemRepo.Lookup(peerCode, idem); lerr == nil && ok {
			var cached transactionpb.SiTxVoteResponse
			if json.Unmarshal([]byte(rec.ResponsePayloadJSON), &cached) == nil {
				return &cached, nil
			}
		}
		return &transactionpb.SiTxVoteResponse{Pending: true}, nil
	case <-time.After(h.receiveSyncDeadline):
		// Slow — leave a pending placeholder (no-op if the worker already
		// cached done) and report 202. The worker keeps running.
		_ = h.idemRepo.UpsertPending(&model.PeerIdempotenceRecord{
			PeerBankCode: peerCode, LocallyGeneratedKey: idem,
			TxRoutingNumber: meta.TxRoutingNumber, TxForeignID: meta.TxForeignID,
			Message: meta.Message, PaymentCode: meta.PaymentCode,
			PaymentPurpose: meta.PaymentPurpose, CallNumber: meta.CallNumber,
		})
		return &transactionpb.SiTxVoteResponse{Pending: true}, nil
	}
}
```

(Confirm `protoToPostings` returns `[]contractsitx.InternalPosting` — it does after the wire-conformance work; if its element type differs, match `runReserve`/`startWorker` signatures to it. Ensure `model` is imported in the handler.)

- [ ] **Step 6: Point `cacheAndReturn` at `UpsertDone`.** In `cacheAndReturn`, replace the `repo.Insert(rec)` call with `repo.UpsertDone(rec)` (set `rec.Status` is handled inside UpsertDone). This unifies the write path so the fast path and the worker both land `done`.

- [ ] **Step 7: Write the failing tests.** In `peer_tx_grpc_handler_test.go` (follow the existing handler-test DB + fake-AccountClient setup). The fake `AccountClient` must be made blockable — add a `reserveGate chan struct{}` to the existing fake (or a new one) whose `ReserveIncoming`/`ReserveOutgoing` block on `<-gate` until released; default (nil gate) = immediate.

```go
func TestHandleNewTx_FastPath_ReturnsVoteNotPending(t *testing.T) {
	h := newTestPeerTxHandler(t, withReceiveDeadline(2*time.Second)) // fast: deadline >> reserve
	resp, err := h.HandleNewTx(ctx, newTxReq("222", "k1", balancedPostings()))
	if err != nil { t.Fatal(err) }
	if resp.GetPending() { t.Fatal("fast reserve must not be pending") }
	if resp.GetType() != "YES" { t.Fatalf("want YES, got %q", resp.GetType()) }
	// record is done
	rec, _, _ := h.idemRepo.Lookup("222", "k1")
	if rec.Status != "done" { t.Fatalf("want done, got %q", rec.Status) }
}

func TestHandleNewTx_SlowReserve_Returns202ThenCaches(t *testing.T) {
	gate := make(chan struct{})
	h := newTestPeerTxHandler(t, withReceiveDeadline(30*time.Millisecond), withReserveGate(gate))

	// First delivery: reserve blocks on the gate, deadline fires → pending.
	resp, err := h.HandleNewTx(ctx, newTxReq("222", "k2", balancedPostings()))
	if err != nil { t.Fatal(err) }
	if !resp.GetPending() { t.Fatal("slow reserve must return pending (202)") }
	rec, found, _ := h.idemRepo.Lookup("222", "k2")
	if !found || rec.Status != "pending" { t.Fatalf("want pending row, got found=%v status=%q", found, rec.Status) }

	// Retransmit while still blocked → still pending, and NO second reserve.
	resp2, _ := h.HandleNewTx(ctx, newTxReq("222", "k2", balancedPostings()))
	if !resp2.GetPending() { t.Fatal("retransmit while blocked must be pending") }

	// Release the reserve; worker upgrades the row to done.
	close(gate)
	waitFor(t, func() bool { r, _, _ := h.idemRepo.Lookup("222", "k2"); return r.Status == "done" })

	// Retransmit after completion → cached vote, not pending.
	resp3, _ := h.HandleNewTx(ctx, newTxReq("222", "k2", balancedPostings()))
	if resp3.GetPending() || resp3.GetType() != "YES" { t.Fatalf("want cached YES, got pending=%v type=%q", resp3.GetPending(), resp3.GetType()) }

	if got := h.fakeClient.reserveIncomingCalls(); got != 1 {
		t.Fatalf("reserve must run exactly once across retransmits, got %d", got)
	}
}

func TestHandleNewTx_RestartRecovery_RekicksWorker(t *testing.T) {
	h := newTestPeerTxHandler(t, withReceiveDeadline(2*time.Second))
	// Simulate a pre-restart pending row with an EMPTY in-flight set.
	_ = h.idemRepo.UpsertPending(&model.PeerIdempotenceRecord{PeerBankCode: "222", LocallyGeneratedKey: "k3", TxForeignID: "tx-3"})
	// A retransmit must re-kick the worker and drive the row to done.
	resp, _ := h.HandleNewTx(ctx, newTxReq("222", "k3", balancedPostings()))
	if !resp.GetPending() { t.Fatal("pending-row retransmit returns pending immediately") }
	waitFor(t, func() bool { r, _, _ := h.idemRepo.Lookup("222", "k3"); return r.Status == "done" })
}
```

Add small helpers in the test file: `withReceiveDeadline`, `withReserveGate`, `waitFor(t, cond)` (polls up to ~2s), `balancedPostings()` (two MONAS legs, one DEBIT one CREDIT, net zero), `newTxReq`. Reuse the existing handler-test scaffolding for the DB and the fake account client; only ADD the blockable gate + a call counter to the fake.

- [ ] **Step 8: Run, verify fail → implement → verify pass.** `cd transaction-service && go test ./internal/handler/ -run TestHandleNewTx -v`. Then the full suite: `go test ./...` (existing HandleCommit/Rollback/GetTxStatus tests must stay green — `cacheAndReturn`→`UpsertDone` is behavior-preserving for them).

- [ ] **Step 9: Lint + commit.**

```bash
cd transaction-service && golangci-lint run ./... && cd ..
git add transaction-service/internal/config/config.go transaction-service/internal/handler/peer_tx_grpc_handler.go transaction-service/cmd/main.go transaction-service/internal/handler/peer_tx_grpc_handler_test.go
git commit -m "feat(sitx): receiver 202 async — timeout-based worker + pending lifecycle + restart re-kick"
```

---

## Task 4: Gateway maps `pending` → HTTP 202

**Files:**
- Modify: `api-gateway/internal/handler/peer_tx_handler.go`
- Test: `api-gateway/internal/handler/peer_tx_handler_test.go`

- [ ] **Step 1: Write the failing test.** Add to `peer_tx_handler_test.go` (the `fakePeerTxClient` already returns a configurable `SiTxVoteResponse`; add a `pending bool` to it, returned from HandleNewTx):

```go
func TestPostInterbank_NewTx_Pending_Returns202(t *testing.T) {
	fake := &fakePeerTxClient{pending: true}
	h := NewPeerTxHandler(fake)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("peer_bank_code", "222")
	c.Request = httptest.NewRequest(http.MethodPost, "/interbank",
		strings.NewReader(`{"idempotenceKey":{"routingNumber":222,"locallyGeneratedKey":"k1"},"messageType":"NEW_TX","message":{"postings":[],"transactionId":{"routingNumber":222,"id":"k1"},"message":"","paymentCode":"","paymentPurpose":""}}`))
	h.PostInterbank(c)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("202 must have empty body, got %s", w.Body.String())
	}
}
```

(Extend `fakePeerTxClient.HandleNewTx` to return `&transactionpb.SiTxVoteResponse{Pending: f.pending, Type: f.voteType}`.)

- [ ] **Step 2: Run, verify fail.** `cd api-gateway && go test ./internal/handler/ -run TestPostInterbank_NewTx_Pending -v` → FAIL (currently renders 200).

- [ ] **Step 3: Implement.** In `PostInterbank`'s `NEW_TX` case, right after the `HandleNewTx` call returns `resp` (and the error check), before building the vote:

```go
	if resp.GetPending() {
		c.Status(http.StatusAccepted) // SI-TX §2.11: accepted, still processing; sender retransmits
		return
	}
```

- [ ] **Step 4: Run, verify pass.** `cd api-gateway && go test ./internal/handler/ -run TestPostInterbank -v` → PASS (existing YES/NO/200 tests stay green).

- [ ] **Step 5: Lint + commit.**

```bash
cd api-gateway && golangci-lint run ./internal/handler/ && cd ..
git add api-gateway/internal/handler/peer_tx_handler.go api-gateway/internal/handler/peer_tx_handler_test.go
git commit -m "feat(sitx): gateway returns 202 when receiver reports pending"
```

---

## Task 5: Docs + config wiring

**Files:**
- Modify: `docs/api/REST_API_v3.md`
- Modify: `docker-compose.yml`

- [ ] **Step 1: REST doc.** In the `POST /interbank` section, the sender-side 202 semantics are already documented. Add a short receiver-side note: "The receiver MAY return **202** for a NEW_TX whose local reserve exceeds `SITX_RECEIVE_SYNC_DEADLINE` (default 5s); it finishes the reserve in the background and the sender's retransmit (same idempotence key) returns the **200** vote once ready. COMMIT_TX/ROLLBACK_TX are always synchronous (204)."

- [ ] **Step 2: docker-compose.** Add `SITX_RECEIVE_SYNC_DEADLINE: "${SITX_RECEIVE_SYNC_DEADLINE:-5s}"` to the `transaction-service` service's `environment:` block in `docker-compose.yml` (match the style of the other `SITX_*`/`INTERBANK_*` vars).

- [ ] **Step 3: Build + commit.**

```bash
make build
git add docs/api/REST_API_v3.md docker-compose.yml
git commit -m "docs(sitx): document receiver 202 async + SITX_RECEIVE_SYNC_DEADLINE"
```

---

## Self-review checklist

- **Spec coverage:** §4.1 worker+race (Task 3), §4.2 upsert rules (Task 1), §4.3 in-flight set (Task 3), §4.4 restart re-kick (Task 3 + test), §4.5 proto/gateway/config (Tasks 2/3/4/5), §6 testing (Tasks 1/3/4). ✔
- **No placeholders:** all steps show code; the only pseudo-names (`newTestIdemRepo`, `newTestPeerTxHandler`, `withReserveGate`, `waitFor`, `balancedPostings`) are explicitly flagged as "use/extend the existing test scaffolding" with what they must do.
- **Type consistency:** `Status` string, `UpsertDone`/`UpsertPending(*model.PeerIdempotenceRecord)`, `runReserve(ctx, peerCode, idem, []contractsitx.InternalPosting, txMeta) *SiTxVoteResponse`, `startWorker(...) chan struct{}`, `GetPending()` used consistently across tasks.

## Risks (from spec §7)

- The worker/deadline race is resolved by the two upsert rules (Task 1). Tasks 1 and 3 carry the concurrency-sensitive tests (single-reserve-across-retransmits, restart re-kick) — review those assertions closely. No change to COMMIT/ROLLBACK correlation or the money path.
