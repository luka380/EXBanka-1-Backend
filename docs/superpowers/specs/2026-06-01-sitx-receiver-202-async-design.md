# SI-TX receiver-side 202 async emission — design

**Date:** 2026-06-01
**Status:** Approved (design)
**Builds on:** `docs/superpowers/specs/2026-05-31-sitx-wire-conformance-design.md` (the wire-conformance pass that landed Tasks 1–19).

## 1. Motivation

SI-TX §2.11 defines three response statuses for `POST /interbank`:

- **202 Accepted** — "the message was accepted and logged locally, but it is not
  done yet. S shall retransmit the message later in order to get a response… This
  allows for transaction steps to be slow (for instance, if the interbank
  coordinator must execute a local saga, it may start it upon receiving the
  message first, and respond with 202 Accepted until it has executed the local
  saga)."
- **200 OK** — done; body is the response (the vote). No redelivery.
- **204 No Content** — done; empty body.

Our **sender** already handles inbound 202 as retry-later (wire-conformance Task
12). Our **receiver** currently only ever returns 200 (synchronous `Reserve`) —
spec-compliant, but if the local reserve is slow it holds the HTTP connection
open until a peer's client times out. This design adds receiver-side 202 so a
slow reserve returns 202 quickly and finishes in the background, with the result
cached for the sender's retransmit.

## 2. Decision (locked during brainstorming)

- **Trigger: timeout-based.** Attempt the reserve synchronously up to a
  configurable deadline; return 200+vote if it finishes in time (fast path
  unchanged), 202 if it exceeds the deadline. NOT always-async (that would add the
  sender's 30–60s retransmit latency to every cross-bank TX).
- **Restart safety: re-kick on retransmit, no new cron.** The sender's retransmit
  is the recovery trigger; the reserve's idempotency + the existing
  reservation-timeout cron bound the risk.
- **Scope: NEW_TX only.** COMMIT/ROLLBACK stay synchronous (fast; 204).

## 3. Current receive path (what this changes)

`transaction-service/internal/handler/peer_tx_grpc_handler.go` `HandleNewTx`:

1. Validate idem/peerCode.
2. **Replay-cache lookup** (`idemRepo.Lookup(peerCode, idem)`) → if found, return
   the cached `SiTxVoteResponse` (this already gives "retransmit returns cached
   vote").
3. `BuildPrelimVote` (cheap balance check) → NO → `cacheAndReturn`.
4. `executor.Reserve(ctx, postings, peerCode, idem)` (synchronous) → NO →
   `cacheAndReturn`; YES → `cacheAndReturn` with a fresh receiver tx UUID.

`cacheAndReturn` INSERTs the `PeerIdempotenceRecord` (with `ResponsePayloadJSON` =
the vote, plus debits/options/meta) and returns the vote. Per §2.11 the record is
committed before the response is sent.

Gateway `api-gateway/internal/handler/peer_tx_handler.go` `PostInterbank`: calls
`HandleNewTx`, renders the vote as **200**.

The replay cache (step 2) is reused as-is for "retransmit returns the cached
result." The new work is splitting "start the reserve" from "return the vote" and
signalling `pending` → HTTP 202.

## 4. Design

### 4.1 Background worker + deadline race

Factor the balance-check + reserve + cache into a worker:

```
func (h *PeerTxGRPCHandler) runReserve(peerCode, idem string, postings, meta) {
    // (background ctx, NOT the request ctx)
    if vote := BuildPrelimVote(postings); vote == NO { upsertDone(NO vote); return }
    res := h.executor.Reserve(bgCtx, postings, peerCode, idem)
    if res == NO { upsertDone(NO vote); return }
    upsertDone(YES vote, txID, debits, options, meta)
}
```

`HandleNewTx` (the not-cached branch):

1. Add `peer:idem` to the in-flight set (mutex-guarded). If already in-flight,
   skip starting a second worker.
2. Start `runReserve` in a goroutine (background context — see 4.4). The goroutine
   removes `peer:idem` from the in-flight set on completion.
3. Race against the deadline:
   - `runReserve` finished before the deadline → re-read the now-cached record and
     return its vote (`pending=false`) → gateway 200. **Fast path: identical
     observable behavior to today; no pending record is ever persisted.**
   - Deadline fires first → `upsertPending(peer, idem)` and return
     `pending=true` → gateway 202. The worker keeps running and upgrades the
     record to done when it finishes.

Implementation of the race: the worker signals completion on a buffered channel;
`select { case <-done: …200…; case <-time.After(deadline): …202… }`.

### 4.2 Record lifecycle + the two upsert rules (race-free)

`PeerIdempotenceRecord` gains `Status string` (`pending` | `done`; existing rows
are effectively `done`). Two repository methods:

- **`UpsertDone(...)`** — `INSERT(status=done, response_payload, debits, options,
  meta, …) ON CONFLICT (peer_bank_code, locally_generated_key) DO UPDATE SET
  status='done', response_payload_json=…, debits_json=…, options_json=…, <meta>`.
  Always lands `done`, overwriting a `pending` row. This replaces today's plain
  INSERT in `cacheAndReturn`.
- **`UpsertPending(peer, idem, meta)`** — `INSERT(status=pending, <meta>) ON
  CONFLICT DO NOTHING`. Creates a `pending` row only if none exists; never
  clobbers a `done` row (handles the worker-cached-just-before-timeout race).

Lookup semantics in `HandleNewTx` step 2:
- record found, `status=done` → return cached vote (200).
- record found, `status=pending` → return `pending=true` (202); if `peer:idem` not
  in the in-flight set, re-kick `runReserve` (restart recovery, 4.4).
- not found → the worker/deadline race above.

Note: `UpsertDone` carrying the debits/options/meta is what `HandleCommitTx` /
`HandleRollbackTx` later read — unchanged; they still `LookupByTransactionID` and
derive reservation keys from `rec.LocallyGeneratedKey`.

### 4.3 In-flight set

A small `sync.Mutex`-guarded `map[string]struct{}` keyed by `peer:idem` on the
handler. Add before starting a worker; delete when the worker returns. Prevents
duplicate concurrent workers for the same TX within one process. Purely an
optimization/guard — correctness does not depend on it (the reserve is
idempotent), but it avoids redundant account-service calls on concurrent
retransmits.

### 4.4 Restart safety

The worker runs under a background context derived from the handler's lifetime
(e.g. `context.Background()` with a generous cap, or the handler's root ctx), NOT
the gRPC request ctx (which is cancelled when `HandleNewTx` returns). On process
restart: in-flight goroutines die, but `pending` rows persist and the in-flight
set is empty. The **sender's next retransmit** finds a `pending` row not in-flight
and re-kicks `runReserve`. Re-running is safe:
- `executor.Reserve` → account-service `ReserveIncoming`/`ReserveOutgoing` are
  idempotent on `peer:idem` (return the existing reservation; never double-book).
- A reservation that is never committed is auto-released by the existing
  reservation-timeout cron, bounding the worst case (sender gives up → receiver's
  hold expires).

No new recovery cron is introduced.

### 4.5 gRPC + gateway + config

- Proto: `SiTxVoteResponse` gains `bool pending = N` (additive; `make proto`).
  When `pending=true`, the `type`/`no_votes` fields are unset/ignored.
- `HandleNewTx` returns `&SiTxVoteResponse{Pending: true}` on the slow path.
- Gateway `PostInterbank` NEW_TX case: `if resp.GetPending() { c.Status(http.
  StatusAccepted); return }` before building the vote JSON; otherwise render the
  vote as 200 (unchanged).
- Config: `transaction-service` adds `ReceiveSyncDeadline time.Duration` from
  `SITX_RECEIVE_SYNC_DEADLINE` (default `5s`). Wire into the handler. Add to
  `docker-compose.yml` api-gateway? No — it's a transaction-service var; add to
  the transaction-service `environment:` block.

## 5. Scope

**In:** NEW_TX 202 path (timeout-based), pending record lifecycle, in-flight
guard, restart-safe re-kick, proto `pending` field, gateway 202 mapping, config.

**Out:** COMMIT/ROLLBACK async (stay synchronous → 204); changing the sender's
retransmit cadence; an always-async mode; a dedicated recovery cron.

## 6. Testing

**Unit (transaction-service handler + repository):**
- Fast reserve (fake executor returns immediately) → `pending=false`, vote
  correct, record `status=done`; assert NO `pending` row was persisted en route
  (or that the final row is `done`).
- Slow reserve (fake executor blocks past a short test deadline) → first call
  returns `pending=true`; a `pending` row exists; a retransmit while still blocked
  → `pending=true` and NO second worker started (assert executor called once);
  after the executor unblocks → record `status=done`, retransmit returns the
  cached vote.
- Restart simulation: pre-insert a `pending` row, empty in-flight set, deliver a
  retransmit → worker re-kicked, completes, row → `done`.
- Concurrency: N concurrent retransmits of the same fresh idem → executor invoked
  once (in-flight guard).
- Repository: `UpsertDone` overwrites a `pending` row; `UpsertPending` is a no-op
  against an existing `done` row.

**Unit (api-gateway handler):**
- `HandleNewTx` returning `pending=true` → `PostInterbank` responds **202** with no
  body; `pending=false` → 200 with the vote (existing tests stay green).

**Integration (`test-app/workflows`, `-tags integration`):** extend or add a case
where a mock peer is the receiver is N/A (we ARE the receiver) — instead, drive a
slow reserve via a test seam or document that the 202 path is unit-covered;
end-to-end 202 requires injecting latency into account-service, which is out of
scope for the integration harness. (Unit coverage is authoritative here.)

## 7. Risks & mitigations

- **Goroutine/record race** (worker completes as the deadline fires): resolved by
  the two upsert rules (4.2) — `UpsertDone` wins (overwrites pending),
  `UpsertPending` never clobbers done.
- **Leaked goroutine on shutdown:** worker uses a background ctx; on shutdown an
  in-flight reserve may be abandoned, leaving a `pending` row → recovered on the
  next retransmit (4.4). Acceptable.
- **Saga fragility:** the reserve logic itself is unchanged — this only changes
  *when* `HandleNewTx` returns relative to the reserve, and adds a record status.
  No change to COMMIT/ROLLBACK correlation, reservation-key derivation, or the
  money path.
- **Deadline too low** → unnecessary 202s (and 30–60s sender latency) for
  borderline-fast TXs. Mitigation: default 5s (well above typical reserve), tunable
  per deployment.
