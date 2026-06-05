# Unified OTC SP-2b (write routes + dispatch in stock-service + my-nid) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use `- [ ]` checkboxes.

**Goal:** One write route per OTC action (bid/counter/accept/reject/cancel/exercise) regardless of local/remote; the gateway is a thin grpc↔rest+auth pass-through; stock-service owns dispatch (local saga vs cross-bank HTTP). Offer reads gain `my_negotiation_id`. Delete `/me/peer-otc/*` + `POST /me/otc/contracts/peer/:id/exercise`.

**Architecture:** Decision A — the cross-bank negotiation HTTP/HMAC egress moves from the gateway's `PeerOTCInitiateHandler` INTO stock-service (which already has `PeerBankAdminServiceClient` + an http.Client). The HMAC signing (identical in `peer_otc_initiate_handler.go` and `transaction-service/internal/sitx/peer_http_client.go`) is extracted to a shared helper. The unified gateway write handlers (`OpenNegotiationChain`/`CounterMyNegotiation`/`AcceptMyNegotiation`/`RejectMyNegotiation`/`CancelMyNegotiation`/`ExerciseContract`) keep their ownership validation + identity resolution and call the SAME stock-service gRPC; stock-service dispatches by `routing_number == OwnRouting()`. Post-SP-2a, remote rows already live in the unified tables, so the remote dispatch writes a remote `OTCNegotiation` row directly (no self-gRPC).

**Tech Stack:** Go, gRPC/proto (`make proto`), Gin gateway, GORM, HMAC-SHA256 peer auth, `test-app/workflows`.

**Spec:** `docs/superpowers/specs/2026-06-05-unified-otc-sp2b-write-routes-design.md`. **Branch:** `feature/unified-otc-sp2b` (already created off Development).

---

## Task 1: Shared HMAC peer-auth helper

**Files:** Create `contract/sitxauth/sign.go` (a tiny dependency-free package both gateway + stock-service + transaction-service can import); Test `contract/sitxauth/sign_test.go`.

The identical block (`X-Api-Key` + optional `X-Bank-Code`/`X-Bank-Signature`/`X-Timestamp`/`X-Nonce` HMAC-SHA256 over the body) appears in 3 places. Extract:
```go
package sitxauth

import (
	"crypto/hmac"; "crypto/rand"; "crypto/sha256"; "encoding/hex"; "net/http"; "time"
)

// Sign sets the SI-TX peer-auth headers on req: always X-Api-Key; and when
// hmacOutboundKey != "", the HMAC bundle (X-Bank-Code = ownBankCode,
// X-Bank-Signature = HMAC-SHA256(body), X-Timestamp RFC3339, X-Nonce).
func Sign(req *http.Request, apiKey, hmacOutboundKey, ownBankCode string, body []byte) {
	req.Header.Set("X-Api-Key", apiKey)
	if hmacOutboundKey == "" {
		return
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	mac := hmac.New(sha256.New, []byte(hmacOutboundKey))
	mac.Write(body)
	req.Header.Set("X-Bank-Code", ownBankCode)
	req.Header.Set("X-Bank-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Timestamp", time.Now().UTC().Format(time.RFC3339))
	req.Header.Set("X-Nonce", hex.EncodeToString(nonce))
}
```
- [ ] Test: a request with hmac key gets all 5 headers + the signature equals `HMAC-SHA256(key, body)`; without a key only `X-Api-Key`.
- [ ] Refactor `transaction-service/internal/sitx/peer_http_client.go` (`postEnvelope` + `CheckStatus`) and (until Task 2 deletes it) leave the gateway as-is, to call `sitxauth.Sign`. Build, test. Commit `refactor(sitx): shared HMAC peer-auth signing helper (SP-2b)`.

## Task 2: stock-service peer-OTC outbound client

**Files:** Create `stock-service/internal/peerotc/client.go` + test.

A client that resolves a peer via `PeerBankAdminServiceClient.ResolvePeerByBankCode` and does `POST/PUT/GET/DELETE {peer.base_url}/negotiations[...]` signed via `sitxauth.Sign`, returning (body, status, err). Mirror the gateway's `CreatePeerNegotiation` POST + `proxyPeerNegotiation` exactly (same payloads/paths). Constructor takes `peerAdmin transactionpb.PeerBankAdminServiceClient`, an `*http.Client`, `ownRouting int64`, `ownBankCode string`.
```go
func (c *Client) CreateNegotiation(ctx, peerBankCode string, offer map[string]any) (routingNumber int64, foreignID string, err error) // POST /negotiations
func (c *Client) Proxy(ctx, peerBankCode, rid, foreignID, method, subpath string, body []byte) (resp []byte, status int, err error) // PUT/GET/DELETE /negotiations/:rid/:id[/accept]
```
- [ ] Test with an `httptest.NewServer` fake peer (assert headers signed, paths correct, response decoded) + a fake `PeerBankAdminServiceClient`. Build/test. Commit `feat(otc): stock-service peer-OTC outbound client (SP-2b)`.

## Task 3: Unified bid dispatch (`OpenNegotiation` local→remote)

**Files:** `contract/proto/stock/stock.proto` (`OpenNegotiationRequest`: add `string bidder_account_number = 12;`), `make proto`; `api-gateway/internal/handler/otc_negotiation_handler.go` `OpenNegotiationChain` (pass the validated bidder account NUMBER); `stock-service/internal/handler/otc_negotiation_handler.go` `OpenNegotiation` (dispatch); wire the peerotc client + account-service into stock-service.

- [ ] Gateway `OpenNegotiationChain`: after `ResolveAndCheckAccount`, it already has the account; fetch its number (the validation `GetAccount` returns it — pass `BidderAccountNumber` into the request). For a CLIENT caller (cross-bank bid is client-to-client in SP-2b; employee/bank bidder is SP-3) the gateway also validates currency-matches-premium when the target is remote — but the gateway no longer knows local/remote. SOLUTION: keep the gateway thin — it always passes account id+number+identity; stock-service does the remote currency/account checks (it has account-service). Move the bidder-account currency check into stock-service's remote branch (it was `CreatePeerNegotiation`'s check).
- [ ] stock-service `OpenNegotiation`: look up parent offer `:id`. If `routing==own` → existing local `OpenNegotiation`. If remote (an `OTCOffer` remote row): compose the SI-TX OtcOffer (buyerId from acting identity → `"client-<owner_id>"`; sellerId from the remote offer's `RemoteSellerID`+routing; terms from the request; `buyerAccountNumber`), validate the bidder account (currency==premium, active, owned — via account-service), `peerotc.CreateNegotiation` to the offer's bank, then `UpsertRemoteNeg` to record the remote `OTCNegotiation` row (routing=peer, native_id=foreignID, the lot key = the remote offer's routing+native_id for cascade). Return the unified `OTCNegotiationResponse` (kind=remote, surrogate id = the upserted row id).
- [ ] Tests: local bid unchanged; remote bid (fake peer) creates the OTCNegotiation remote row + returns it; bad bidder account/currency → error. Build/test/lint. Commit `feat(otc): bid route dispatches local+cross-bank in stock-service (SP-2b)`.

## Task 4: Unified counter/accept/reject/cancel dispatch

**Files:** `stock-service/internal/handler/otc_negotiation_handler.go` (`CounterNegotiation`/`AcceptNegotiationChain`/`RejectNegotiation`/`CancelNegotiation` dispatch); the peerotc client.

For each: look up negotiation `:nid`. If `routing==own` → existing local path. If remote (an `OTCNegotiation` remote row): derive the counterparty bank code + `(rid, foreignID)` from the row (`routing_number`/`native_id`), and:
- Counter → `peerotc.Proxy(PUT, "")` with the OtcOffer body composed from the request; then `UpdateRemoteNegOffer` mirror.
- Accept → `peerotc.Proxy(GET, "/accept")`; then `CompareAndSetRemoteNegStatus(ongoing→accepted)` + cross-bank cascade-cancel (reuse the existing `CascadeCancelSiblings` over remote OTCNegotiation rows + fire `peerotc.Proxy(DELETE)` to each sibling bidder bank). Return the accept response shape.
- Reject/Cancel → `peerotc.Proxy(DELETE, "")`; then `UpdateRemoteNegStatus(cancelled)` mirror.
The `(rid, foreignID)` come from the row, NEVER the client (ownership: the row is the caller's chain — verify the caller is the chain's bidder/party before dispatching, mirroring `resolveMyRoleAndPeer`).
- [ ] Tests: local counter/accept/reject/cancel unchanged; remote ones (fake peer) proxy correctly + mirror the status. Build/test/lint. Commit `feat(otc): counter/accept/reject/cancel dispatch local+cross-bank in stock-service (SP-2b)`.

## Task 5: Unified exercise dispatch

**Files:** `stock-service/internal/handler/otc_options_handler.go` `ExerciseContract`; gateway `ExerciseContract` (carry buyer account number for the remote path).

- [ ] stock-service `ExerciseContract`: look up contract `:id`. If `routing==own` → local exercise saga (existing). If remote → the existing `InitiateOptionExercise` logic (move/share it) using the contract's stored buyer account + dispatch via transaction-service. Gateway `ExerciseContract` keeps the buyer-account ownership gate (currently in `ExercisePeerContract`) and passes the account number for the remote path.
- [ ] Tests: local exercise unchanged; remote exercise dispatches the SI-TX. Build/test/lint. Commit `feat(otc): exercise route dispatches local+cross-bank in stock-service (SP-2b)`.

## Task 6: my_negotiation_id on offer reads

**Files:** proto (`UnifiedOptionOffer` + `OTCOfferResponse`: add `uint64 my_negotiation_id = N; string my_negotiation_status = N+1;`), `make proto`; `stock-service` `ListUnifiedOptionOffers` + `GetOffer`.

- [ ] In both read paths: given the acting identity, fetch the caller's negotiation chains (`ListByBidder` local + `ListRemoteNegByClient` remote) and build a map `parent_offer_id → (nid, status)` (local) and `(remote parent routing+native) → (nid, status)` (remote). Stamp `my_negotiation_id`/`my_negotiation_status` on each offer the caller has a chain on; absent (0/"") otherwise. The gateway passes the fields through.
- [ ] Tests: an offer the caller bid on returns my_negotiation_id = their chain's surrogate id; an offer they didn't bid on omits it; works for a remote offer too. Build/test/lint. Commit `feat(otc): my_negotiation_id on offer reads so FE finds its own chain (SP-2b)`.

## Task 7: Delete the peer routes + handler + orphaned proto

**Files:** `api-gateway/internal/router/router_v3.go` (delete the 5 `/me/peer-otc/negotiations*` routes + `POST /me/otc/contracts/peer/:id/exercise`); delete `api-gateway/internal/handler/peer_otc_initiate_handler.go` (+ its test) and the gateway `OTCOptionsHandler.ExercisePeerContract` method; remove the now-unused gateway wiring (`PeerOTCInitiate` from handlers/deps); `contract/proto/stock/stock.proto` remove `ListContractsResponse.peer_contracts`/`peer_total`, `make proto`, and drop any `PeerOTCService` RPCs now unused by the gateway (RecordOutboundNegotiation etc. — verify each is unused before deleting; the inbound peer handlers may still use some).

- [ ] `grep -rn "peer-otc\|PeerOTCInitiate\|ExercisePeerContract\|peer_contracts\|PeerContracts" api-gateway/` → only deletions remain. Build all modules. Commit `refactor(otc): delete /me/peer-otc/* + peer-exercise routes, retire gateway PeerOTCInitiateHandler + orphaned proto (SP-2b clean-cut)`.

## Task 8: Docs, swagger, version, integration, dead-code sweep, full verify

- [ ] `docs/api/REST_API_v3.md`: remove the deleted routes; document that bid/counter/accept/reject/cancel/exercise + offer reads handle local+remote uniformly; `my_negotiation_id` field. `Specification.md` §3 gateway-client wiring + §17 routes updated. `make swagger`.
- [ ] `VERSION` → `2.0.0` (MAJOR — the `/me/peer-otc/*` + peer-exercise route deletions are the authorized breaking cut) + `version.go` in sync.
- [ ] Integration (`test-app/workflows`): unified bid→counter→accept on a LOCAL offer end-to-end (FE-style, ids from responses); the deleted `/me/peer-otc/*` routes return 404; `my_negotiation_id` present on an offer the caller bid on; cross-bank parts `t.Skip` without a 2nd stack.
- [ ] Dead-code sweep: `grep -rn --include="*.go" "PeerOTCInitiateHandler\|CreatePeerNegotiation\|proxyPeerNegotiation\|ExercisePeerContract"` → empty; `golangci-lint run ./...` clean across services.
- [ ] `make build 2>&1 | tail`, `make lint 2>&1 | tail`, `make test 2>&1 | tail` — REAL output, all green. Commit `test+docs(otc): SP-2b docs, VERSION 2.0.0, integration tests, dead-code sweep`.

---

## Self-review notes
- Spec coverage: dispatch relocation (T1-T5), my-nid (T6), route deletion + proto cleanup (T7), docs/version/verify (T8). Frozen `/cross-bank-protocol/*` untouched (we only CALL it from stock-service now instead of the gateway).
- Ownership preserved: gateway still validates the caller's account before forwarding (T3/T5); for remote, stock-service re-validates currency/account (moved from `CreatePeerNegotiation`).
- Risk: the bid/counter/accept remote branches must reproduce `CreatePeerNegotiation`/`proxyPeerNegotiation` semantics exactly (payloads, parent-offer cascade key, accept cascade-cancel). Each has a fake-peer test.
- SP-3 (employee-`<N>` wire identity + bank settlement + conformant bank-owned offer exposure) builds on this; SP-2b's cross-bank bid handles the CLIENT buyer case.
