# Interbank-service Cutover Plan — wire it in *without* a flag-day

> Goal: route the entire cross-bank protocol through `interbank-service`, replacing the cross-bank functions in stock-service (OTC egress) and transaction-service (TX engine). Strategy: **make everything ready behind a config switch, then flip it** — both the old and new paths coexist so the cutover is a single env change + restart, instantly reversible, with no code redeploy at switch time.

## Principle: a switch, not a rewrite
Every wiring change is gated by a config flag so the running system is unchanged until you flip it:
- **Gateway:** `INTERBANK_GRPC_ADDR` set ⇒ dial *all* `/cross-bank-protocol` peer clients at interbank; unset ⇒ today's behavior (PeerTx/PeerBankAdmin→transaction-service, PeerOTC→stock-service, /user→client/user directly).
- **stock-service:** `INTERBANK_EGRESS_ADDR` set ⇒ outbound peer HTTP goes through `PeerEgressService.ProxyToPeer`; unset ⇒ today's direct `peerotc.Client` + `otccache` HTTP.

Flip = set the env on the gateway (+ stock-service) and restart those two. Rollback = unset + restart.

---

## Phase 0 — DONE (committed)
- `interbank-service` built: serves the whole `/cross-bank-protocol` surface (PeerTx engine + PeerBankAdmin registry + PeerEgress egress/reachability/state; PeerOTC forwarder→stock; PeerUser resolver→client/user). gRPC `:50062`, ops HTTP `:9108`.
- CI (`ci.yml` build/test/lint/tidy) + CD (`cd.yml` build/publish) + `docker-compose.yml` (interbank-service + interbank-db `:5443`, dormant) include it.

## Phase 1 — MAKE READY (code in place behind flags; nothing routes yet)

**1a. Gateway flag-gated peer clients** (`api-gateway`)
- Add `INTERBANK_GRPC_ADDR` config. When set, dial one connection to interbank and back **all** peer clients with it: `PeerTxServiceClient`, `PeerBankAdminServiceClient`, `PeerOTCServiceClient`, `PeerEgressServiceClient`, and a new `PeerUserServiceClient`. When unset, keep the current per-service dials. (The `PeerTxHandler`/`PeerOTCHandler` handler code is unchanged — only the client wiring in `router/handlers.go` + `main.go` changes.)
- `PeerUserHandler`: give it an interbank path — when the flag is on, call `PeerUserService.ResolvePeerUser` and map `found=false`→404, else compose `{bankDisplayName, displayName}`; when off, keep the direct client/user lookups. (Both paths coexist; flag picks.)
- Inbound peer auth (PeerAuth middleware) resolves peers via `PeerBankAdminService.ResolvePeerByAPIToken` — it follows the same flag (interbank vs transaction-service).

**1b. stock-service flag-gated egress** (`stock-service`)
- Add `INTERBANK_EGRESS_ADDR` config + a `PeerEgressServiceClient`.
- New `PeerNegotiationDispatcher` impl backed by `ProxyToPeer`: `CreateNegotiation(code, offer)` → `ProxyToPeer(code,"POST","/negotiations",body)` (parse `{routingNumber,id}`); `Proxy(code,rid,fid,method,subpath,body)` → `ProxyToPeer(code,method,"/negotiations/"+rid+"/"+fid+subpath,body)`. Wire it when the flag is set; else the current `peerotc.Client`.
- `otccache` (`cache.go`, `option_cache.go`): when the flag is set, fetch `/public-stock` + `/public-option-offers` via `ProxyToPeer(code,"GET",path,nil)` (enumerate peers via `PeerBankAdminService.ListPeerBanks` on interbank); else the current direct HTTP.

**1c. Infra (your deploy gate)**
- Provision `interbank_db` (Postgres, `:5443`) + Helm/k8s manifest for interbank-service (gRPC 50062, ops 9108, env, `depends_on` account/stock/client/user + db — **not** a hard cycle with stock).
- Seed `interbank_db.peer_banks` from transaction-service's `peer_banks` (copy rows, or re-register peers via `POST /api/v3/peer-banks` once the gateway flag is on).
- Deploy interbank-service **dormant** (flags still off) and smoke-test: `/readyz` green, `GetPeersState` reports the registered peers reachable.

## Phase 2 — SWITCH (one env change + restart; reversible)
1. Set `INTERBANK_GRPC_ADDR` on the **api-gateway** and restart it → all inbound `/cross-bank-protocol` now hits interbank-service.
2. Set `INTERBANK_EGRESS_ADDR` on **stock-service** and restart it → all outbound OTC/discovery peer HTTP now goes through interbank.
3. Verify end-to-end against a peer (or the second local stack): discovery, a full negotiation (bid→counter→accept→settle), a cross-bank transfer, `GET /user`, and `GetTxStatus`. Watch interbank `/metrics` + logs.
4. **Rollback if needed:** unset the two envs, restart the two services — back to the old paths instantly (no redeploy).

## Phase 3 — CLEANUP (after the switch is stable)
- Remove the SI-TX engine from transaction-service (the 14 ported files), drop its `peer_banks`/`peer_idempotence_records`/`outbound_peer_txs` from AutoMigrate, stop registering `PeerTxService`/`PeerBankAdminService` + the two crons. transaction-service → pure local payments/transfers/fees.
- Remove stock-service's direct `peerotc.Client` + `otccache` HTTP egress (now dead behind the flag).
- Drop the flags (make interbank the only path) once you're confident.
- Bump `VERSION` (the switch is the behavior-affecting change) at Phase 2.

## Notes / risks
- **No flag-day:** old + new paths coexist through Phase 1; Phase 2 is a per-service env flip with instant rollback.
- **interbank↔stock cycle is fine** (lazy gRPC dials, no compile cycle): interbank forwards OTC to stock + calls stock for option legs; stock calls interbank for egress.
- **One inbound hop added** for OTC (gateway→interbank→stock) — negligible at cohort scale.
- **Idempotency/peer_banks data migration** is the only stateful step — do it before flipping the gateway flag so inbound auth + outbound signing resolve peers.
- Effort: gateway flag wiring ~2–3h; stock-service dispatcher+otccache ~half a day; infra/Helm/data = your deploy gate; cleanup ~half a day.
