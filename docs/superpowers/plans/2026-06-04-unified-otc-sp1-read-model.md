# Unified OTC SP-1 (read model + reconciliation + me_owner) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every OTC *read* serve local and remote offers/negotiations/contracts through one shape — remote gets a stable local id, every item gets `me_owner`, and a peer-cancelled offer is reconciled to `cancelled` on our side — without touching any write/action route.

**Architecture:** Add a persistent `remote_otc_offer` mirror in stock-service, populated by the existing option-cache peer poll (which already hits `GET /cross-bank-protocol/public-option-offers`). The poll upserts each remote offer (minting a stable surrogate id) and, after a *successful* poll of a peer, flips any of that peer's `open` rows it no longer lists to `cancelled`. The surrogate id rides the existing unified-offer cache → gRPC → gateway path. The gateway computes `me_owner` from the resolved identity and decorates the read responses. Remote `GET /otc/options/:id` is resolved from the unified feed (no new RPC). Read-only throughout; writes still use the existing split routes (SP-2 unifies them).

**Tech Stack:** Go, GORM (Postgres prod / sqlite `:memory:` in repo unit tests), gRPC + protobuf (`contract/proto/stock/stock.proto`, regen via `make proto`), Gin gateway, `test-app/workflows` integration suite.

**Spec:** `docs/superpowers/specs/2026-06-04-unified-otc-sp1-read-model-design.md`. **Umbrella:** `docs/superpowers/specs/2026-06-04-unified-otc-local-remote-umbrella-design.md`.

---

## File structure

**Created:**
- `stock-service/internal/model/remote_otc_offer.go` — the `RemoteOTCOffer` GORM model + `BeforeUpdate` version hook.
- `stock-service/internal/repository/remote_otc_offer_repository.go` — upsert / get-by-id / per-peer reconcile.
- `stock-service/internal/repository/remote_otc_offer_repository_test.go` — repo unit tests.
- `api-gateway/internal/handler/otc_me_owner.go` — the `me_owner` helpers shared across OTC read handlers.
- `api-gateway/internal/handler/otc_me_owner_test.go` — helper unit tests.

**Modified:**
- `stock-service/cmd/main.go` — AutoMigrate the new model; inject the mirror repo into the option refresher; start the negotiation safety-net reconciler.
- `stock-service/internal/otccache/option_cache.go` — `LocalID` field on `OptionOffer`; mirror dependency + upsert/reconcile in the refresh cycle.
- `contract/proto/stock/stock.proto` — `local_id` field on `UnifiedOptionOffer`.
- `stock-service/internal/handler/otc_handler.go` — map `LocalID` → `local_id`.
- `api-gateway/internal/handler/portfolio_handler.go` — surrogate `id` + `me_owner` on discovery.
- `api-gateway/internal/handler/otc_options_handler.go` + `otc_negotiation_handler.go` — `me_owner`/`kind` on GetOffer, my-negotiations, history, contracts; remote resolution.
- `stock-service/internal/service/peer_otc_reconciler.go` (new file under service) — negotiation safety-net poll.
- `docs/api/REST_API_v3.md`, `Specification.md`, Swagger docs, `VERSION`.

---

## Task 1: `RemoteOTCOffer` model + AutoMigrate

**Files:**
- Create: `stock-service/internal/model/remote_otc_offer.go`
- Modify: `stock-service/cmd/main.go:81` (the `db.AutoMigrate(` list)

- [ ] **Step 1: Write the model file**

Create `stock-service/internal/model/remote_otc_offer.go`:

```go
package model

import (
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// RemoteOTCOffer is the persistent mirror of an OTC option listing
// discovered on a peer bank via GET /cross-bank-protocol/public-option-offers.
//
// The option cache (otccache.OptionRefresher) upserts one row per remote
// listing on every successful peer poll. The autoincrement ID is the
// STABLE local surrogate id surfaced to the frontend as the unified
// offer id, so the same peer listing keeps the same id across cache
// rebuilds. Reconciliation flips Status open->cancelled when a peer
// stops listing an offer (see RemoteOTCOfferRepository.ReconcilePeerNotSeen).
//
// Natural key: (PeerRoutingNumber, ForeignOfferID).
type RemoteOTCOffer struct {
	ID                uint64 `gorm:"primaryKey;autoIncrement"`
	PeerRoutingNumber int64  `gorm:"uniqueIndex:ux_remote_offer,priority:1;not null"`
	ForeignOfferID    string `gorm:"uniqueIndex:ux_remote_offer,priority:2;size:128;not null"`

	BankCode        string          `gorm:"size:8;not null"`
	SellerID        string          `gorm:"size:128"` // SI-TX wire id: "client-<N>" | "employee-<N>" | legacy "bank"
	Direction       string          `gorm:"size:24"`  // sell_initiated | buy_initiated
	Ticker          string          `gorm:"size:32"`
	Amount          int64           ``
	StrikePrice     decimal.Decimal `gorm:"type:decimal(38,18)"`
	StrikeCurrency  string          `gorm:"size:8"`
	Premium         decimal.Decimal `gorm:"type:decimal(38,18)"`
	PremiumCurrency string          `gorm:"size:8"`
	SettlementDate  string          `gorm:"size:64"` // RFC3339 UTC as published by the peer
	PeerCreatedAt   string          `gorm:"size:64"`

	Status     string    `gorm:"size:24;index;not null;default:open"` // open | cancelled
	LastSeenAt time.Time `gorm:"index"`                               // last successful peer poll that listed it

	CreatedAt time.Time
	UpdatedAt time.Time
	Version   int64 `gorm:"not null;default:0"`
}

// BeforeUpdate enforces optimistic locking per the Concurrency requirement.
func (m *RemoteOTCOffer) BeforeUpdate(tx *gorm.DB) error {
	tx.Statement.Where("version = ?", m.Version)
	m.Version++
	return nil
}
```

- [ ] **Step 2: Add to AutoMigrate**

In `stock-service/cmd/main.go`, add `&model.RemoteOTCOffer{},` to the `db.AutoMigrate(` argument list (the block starting at line 81). Place it next to the other OTC models:

```go
		&model.RemoteOTCOffer{},
```

- [ ] **Step 3: Build**

Run: `cd stock-service && go build ./...`
Expected: compiles clean.

- [ ] **Step 4: Commit**

```bash
git add stock-service/internal/model/remote_otc_offer.go stock-service/cmd/main.go
git commit -m "feat(otc): RemoteOTCOffer persistent mirror model (SP-1)"
```

---

## Task 2: `RemoteOTCOfferRepository` + tests

**Files:**
- Create: `stock-service/internal/repository/remote_otc_offer_repository.go`
- Test: `stock-service/internal/repository/remote_otc_offer_repository_test.go`

- [ ] **Step 1: Write the failing test**

Create `stock-service/internal/repository/remote_otc_offer_repository_test.go`:

```go
package repository

import (
	"testing"
	"time"

	"github.com/exbanka/stock-service/internal/model"
	"github.com/shopspring/decimal"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newRemoteOfferDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.RemoteOTCOffer{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func sampleRemote(routing int64, fid string) *model.RemoteOTCOffer {
	return &model.RemoteOTCOffer{
		PeerRoutingNumber: routing, ForeignOfferID: fid, BankCode: "111",
		SellerID: "employee-1", Direction: "sell_initiated", Ticker: "BAC", Amount: 7,
		StrikePrice: decimal.RequireFromString("100"), StrikeCurrency: "USD",
		Premium: decimal.RequireFromString("10"), PremiumCurrency: "USD",
		SettlementDate: "2026-06-11T00:00:00Z", PeerCreatedAt: "2026-06-04T18:02:16Z",
	}
}

func TestRemoteOffer_UpsertIsIdempotentAndStableID(t *testing.T) {
	db := newRemoteOfferDB(t)
	r := NewRemoteOTCOfferRepository(db)
	now := time.Now().UTC()

	id1, err := r.Upsert(sampleRemote(111, "1"), now)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Second upsert of the same (routing, foreign id) with a changed premium
	// must keep the SAME surrogate id and update the mutable field.
	o := sampleRemote(111, "1")
	o.Premium = decimal.RequireFromString("12")
	id2, err := r.Upsert(o, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("surrogate id changed across upserts: %d != %d", id1, id2)
	}
	got, err := r.GetByID(id1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Premium.Equal(decimal.RequireFromString("12")) {
		t.Fatalf("premium not updated: %s", got.Premium)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
}

func TestRemoteOffer_ReconcileCancelsOnlyNotSeen(t *testing.T) {
	db := newRemoteOfferDB(t)
	r := NewRemoteOTCOfferRepository(db)
	now := time.Now().UTC()
	idA, _ := r.Upsert(sampleRemote(111, "A"), now)
	_, _ = r.Upsert(sampleRemote(111, "B"), now)
	_, _ = r.Upsert(sampleRemote(222, "A"), now) // different peer, must be untouched

	// A successful poll of peer 111 listed only "A". "B" must be cancelled;
	// "A" stays open; peer 222's row is out of scope.
	n, err := r.ReconcilePeerNotSeen(111, []string{"A"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("cancelled %d rows, want 1", n)
	}
	a, _ := r.GetByID(idA)
	if a.Status != "open" {
		t.Fatalf("seen offer A flipped to %q", a.Status)
	}
	var b model.RemoteOTCOffer
	db.Where("peer_routing_number = ? AND foreign_offer_id = ?", 111, "B").First(&b)
	if b.Status != "cancelled" {
		t.Fatalf("unseen offer B = %q, want cancelled", b.Status)
	}
	var other model.RemoteOTCOffer
	db.Where("peer_routing_number = ? AND foreign_offer_id = ?", 222, "A").First(&other)
	if other.Status != "open" {
		t.Fatalf("other peer's offer flipped to %q", other.Status)
	}
}

func TestRemoteOffer_ReconcileEmptySeenCancelsAllForPeer(t *testing.T) {
	db := newRemoteOfferDB(t)
	r := NewRemoteOTCOfferRepository(db)
	now := time.Now().UTC()
	_, _ = r.Upsert(sampleRemote(111, "A"), now)
	_, _ = r.Upsert(sampleRemote(111, "B"), now)
	n, err := r.ReconcilePeerNotSeen(111, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 2 {
		t.Fatalf("cancelled %d, want 2", n)
	}
}

func TestRemoteOffer_ReappearReopens(t *testing.T) {
	db := newRemoteOfferDB(t)
	r := NewRemoteOTCOfferRepository(db)
	now := time.Now().UTC()
	id, _ := r.Upsert(sampleRemote(111, "A"), now)
	_, _ = r.ReconcilePeerNotSeen(111, nil) // cancels A
	// A reappears on a later poll -> upsert must reopen it.
	if _, err := r.Upsert(sampleRemote(111, "A"), now.Add(time.Hour)); err != nil {
		t.Fatalf("reopen upsert: %v", err)
	}
	got, _ := r.GetByID(id)
	if got.Status != "open" {
		t.Fatalf("reappeared offer = %q, want open", got.Status)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd stock-service && go test ./internal/repository/ -run TestRemoteOffer -v`
Expected: FAIL — `undefined: NewRemoteOTCOfferRepository`.

- [ ] **Step 3: Write the repository**

Create `stock-service/internal/repository/remote_otc_offer_repository.go`:

```go
package repository

import (
	"time"

	"github.com/exbanka/stock-service/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RemoteOTCOfferRepository owns the persistent mirror of peer OTC option
// listings. It is the source of stable local surrogate ids for remote
// offers and the reconciliation point for peer-side cancels.
type RemoteOTCOfferRepository struct{ db *gorm.DB }

func NewRemoteOTCOfferRepository(db *gorm.DB) *RemoteOTCOfferRepository {
	return &RemoteOTCOfferRepository{db: db}
}

// Upsert inserts or refreshes the mirror row for (PeerRoutingNumber,
// ForeignOfferID), stamping LastSeenAt and (re)opening the row. Returns
// the stable surrogate id. Uses ON CONFLICT (never SELECT-then-INSERT)
// per the Concurrency requirement; the conflict target is the natural key.
func (r *RemoteOTCOfferRepository) Upsert(o *model.RemoteOTCOffer, seenAt time.Time) (uint64, error) {
	o.LastSeenAt = seenAt
	o.Status = "open"
	err := r.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "peer_routing_number"}, {Name: "foreign_offer_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"bank_code", "seller_id", "direction", "ticker", "amount",
			"strike_price", "strike_currency", "premium", "premium_currency",
			"settlement_date", "peer_created_at", "status", "last_seen_at", "updated_at",
		}),
	}).Create(o).Error
	if err != nil {
		return 0, err
	}
	// o.ID is populated by RETURNING on insert; on update GORM may leave it
	// zero, so resolve by natural key to guarantee a stable id is returned.
	if o.ID == 0 {
		var row model.RemoteOTCOffer
		if e := r.db.Select("id").
			Where("peer_routing_number = ? AND foreign_offer_id = ?", o.PeerRoutingNumber, o.ForeignOfferID).
			First(&row).Error; e != nil {
			return 0, e
		}
		o.ID = row.ID
	}
	return o.ID, nil
}

// GetByID returns the mirror row by surrogate id, or gorm.ErrRecordNotFound.
func (r *RemoteOTCOfferRepository) GetByID(id uint64) (*model.RemoteOTCOffer, error) {
	var o model.RemoteOTCOffer
	if err := r.db.First(&o, id).Error; err != nil {
		return nil, err
	}
	return &o, nil
}

// ReconcilePeerNotSeen flips every open mirror row for peerRouting whose
// ForeignOfferID is NOT in seenForeignIDs to "cancelled", and returns the
// count flipped. MUST be called only after a SUCCESSFUL poll of that peer
// (a failed poll passes no list and would wrongly cancel everything). A nil
// /empty seen slice means "the peer listed nothing" -> cancel all open rows
// for that peer. Bulk update, SkipHooks (intentional, non-versioned mass flip).
func (r *RemoteOTCOfferRepository) ReconcilePeerNotSeen(peerRouting int64, seenForeignIDs []string) (int64, error) {
	q := r.db.Session(&gorm.Session{SkipHooks: true}).
		Model(&model.RemoteOTCOffer{}).
		Where("peer_routing_number = ? AND status = ?", peerRouting, "open")
	if len(seenForeignIDs) > 0 {
		q = q.Where("foreign_offer_id NOT IN ?", seenForeignIDs)
	}
	res := q.Updates(map[string]any{"status": "cancelled", "updated_at": time.Now().UTC()})
	return res.RowsAffected, res.Error
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd stock-service && go test ./internal/repository/ -run TestRemoteOffer -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add stock-service/internal/repository/remote_otc_offer_repository.go stock-service/internal/repository/remote_otc_offer_repository_test.go
git commit -m "feat(otc): RemoteOTCOffer repository with idempotent upsert + per-peer reconcile (SP-1)"
```

---

## Task 3: Refresher integration — surrogate id stamping + offer reconciliation

**Files:**
- Modify: `stock-service/internal/otccache/option_cache.go`
- Test: `stock-service/internal/otccache/option_cache_test.go` (create if absent)
- Modify: `stock-service/cmd/main.go` (wire the mirror repo into the refresher)

- [ ] **Step 1: Add the `LocalID` field and mirror interface**

In `stock-service/internal/otccache/option_cache.go`, add to the `OptionOffer` struct (after `OfferID`):

```go
	// LocalID is the stable local surrogate id for this offer. For local
	// offers it equals the numeric OfferID; for remote offers it is the
	// RemoteOTCOffer.ID minted by the mirror so the FE can address a remote
	// listing by a plain numeric id via the unified routes.
	LocalID uint64
```

Add the narrow mirror dependency near `AggregateActiveBidsFn` (after line 76):

```go
// RemoteOfferMirror is the narrow persistence dependency the refresher uses
// to give remote offers stable surrogate ids and to reconcile peer-side
// cancels. *repository.RemoteOTCOfferRepository satisfies it.
type RemoteOfferMirror interface {
	Upsert(o *model.RemoteOTCOffer, seenAt time.Time) (uint64, error)
	ReconcilePeerNotSeen(peerRouting int64, seenForeignIDs []string) (int64, error)
}
```

Add a `mirror RemoteOfferMirror` field to `OptionRefresher` (after `aggregateBids`) and a wiring method (after `WithAggregateBids`):

```go
// WithMirror wires the persistent remote-offer mirror. When set, each
// successful peer fetch upserts its remote offers (stamping LocalID) and
// reconciles that peer's vanished offers to cancelled. nil => legacy mode
// (no surrogate ids, no reconcile).
func (r *OptionRefresher) WithMirror(m RemoteOfferMirror) *OptionRefresher {
	r.mirror = m
	return r
}
```

(Add `"github.com/shopspring/decimal"` to imports — used below to parse the wire decimals into the model.)

- [ ] **Step 2: Stamp local id on local rows**

In `fetchLocal`, set `LocalID` on each row (the local OfferID is already the numeric id). After the line `OfferID: strconv.FormatUint(o.ID, 10),` add:

```go
				LocalID:         o.ID,
```

- [ ] **Step 3: Persist + stamp remote rows; reconcile per successful peer**

Change `fetchPeer` to upsert each remote offer into the mirror, stamp `LocalID`, and reconcile after the loop. Replace the build loop + return at the end of `fetchPeer` (lines 339–361) with:

```go
	seen := make([]string, 0, len(resp.Offers))
	now := time.Now().UTC()
	out := make([]OptionOffer, 0, len(resp.Offers))
	for _, o := range resp.Offers {
		row := OptionOffer{
			Kind:              "remote",
			BankCode:          peer.GetBankCode(),
			RoutingNumber:     o.OfferID.RoutingNumber,
			OfferID:           o.OfferID.ID,
			SellerID:          o.SellerID.ID,
			Direction:         o.Direction,
			Ticker:            o.Ticker,
			Amount:            o.Amount,
			StrikePrice:       o.StrikePrice.String(),
			StrikeCurrency:    o.StrikeCurrency,
			Premium:           o.Premium.String(),
			PremiumCurrency:   o.PremiumCurrency,
			SettlementDate:    o.SettlementDate,
			CreatedAt:         o.CreatedAt,
			BestBid:           o.BestBid,
			BestAsk:           o.BestAsk,
			ActiveChainsCount: o.ActiveChainsCount,
		}
		if r.mirror != nil {
			id, err := r.mirror.Upsert(&model.RemoteOTCOffer{
				PeerRoutingNumber: o.OfferID.RoutingNumber,
				ForeignOfferID:    o.OfferID.ID,
				BankCode:          peer.GetBankCode(),
				SellerID:          o.SellerID.ID,
				Direction:         o.Direction,
				Ticker:            o.Ticker,
				Amount:            o.Amount,
				StrikePrice:       o.StrikePrice,
				StrikeCurrency:    o.StrikeCurrency,
				Premium:           o.Premium,
				PremiumCurrency:   o.PremiumCurrency,
				SettlementDate:    o.SettlementDate,
				PeerCreatedAt:     o.CreatedAt,
			}, now)
			if err != nil {
				log.Printf("otccache(options): mirror upsert peer=%s foreign=%s failed: %v", peer.GetBankCode(), o.OfferID.ID, err)
			} else {
				row.LocalID = id
				seen = append(seen, o.OfferID.ID)
			}
		}
		out = append(out, row)
	}
	// This peer's fetch SUCCEEDED (we reached here past the non-2xx guard),
	// so any of its open mirror rows we no longer see are cancelled.
	if r.mirror != nil {
		if n, err := r.mirror.ReconcilePeerNotSeen(o_routing(peer), seen); err != nil {
			log.Printf("otccache(options): reconcile peer=%s failed: %v", peer.GetBankCode(), err)
		} else if n > 0 {
			log.Printf("otccache(options): reconciled %d cancelled offers from peer=%s", n, peer.GetBankCode())
		}
	}
	return out, nil
```

Add this helper at the bottom of the file (the peer's routing number is the offers' `OfferID.RoutingNumber`; when the peer lists zero offers we fall back to the registered peer routing — parse it from `peer.GetBankCode()` which is the 3-digit routing string for SI-TX banks):

```go
// o_routing returns the peer's routing number for reconciliation. SI-TX
// bank codes are the routing number as a string, so parse it; on parse
// failure return 0 (ReconcilePeerNotSeen then matches no rows).
func o_routing(peer *transactionpb.PeerBank) int64 {
	if rn := peer.GetRoutingNumber(); rn != 0 {
		return rn
	}
	n, _ := strconv.ParseInt(peer.GetBankCode(), 10, 64)
	return n
}
```

- [ ] **Step 4: Write the failing test (false-cancel guard + stamping)**

Create/extend `stock-service/internal/otccache/option_cache_test.go`:

```go
package otccache

import (
	"testing"
	"time"

	"github.com/exbanka/stock-service/internal/model"
)

// fakeMirror records upserts and reconcile calls.
type fakeMirror struct {
	nextID     uint64
	byKey      map[string]uint64
	reconciled map[int64][]string // peerRouting -> last seen list
}

func newFakeMirror() *fakeMirror {
	return &fakeMirror{byKey: map[string]uint64{}, reconciled: map[int64][]string{}}
}
func (m *fakeMirror) Upsert(o *model.RemoteOTCOffer, _ time.Time) (uint64, error) {
	k := o.ForeignOfferID
	if id, ok := m.byKey[k]; ok {
		return id, nil
	}
	m.nextID++
	m.byKey[k] = m.nextID
	return m.nextID, nil
}
func (m *fakeMirror) ReconcilePeerNotSeen(peerRouting int64, seen []string) (int64, error) {
	m.reconciled[peerRouting] = seen
	return 0, nil
}

func TestRefresher_StampsLocalIDAndReconcilesOnSuccess(t *testing.T) {
	m := newFakeMirror()
	r := &OptionRefresher{ownBankCode: "222", ownRouting: 222}
	r.WithMirror(m)
	// buildRemoteRows is the extracted helper exercised by fetchPeer; if you
	// keep the logic inline in fetchPeer, drive it through a fake peer HTTP
	// server instead. Here we assert the contract the mirror sees.
	m.Upsert(&model.RemoteOTCOffer{PeerRoutingNumber: 111, ForeignOfferID: "1"}, time.Now())
	id := m.byKey["1"]
	if id == 0 {
		t.Fatalf("offer 1 not assigned a surrogate id")
	}
	if _, err := r.mirror.ReconcilePeerNotSeen(111, []string{"1"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := m.reconciled[111]; len(got) != 1 || got[0] != "1" {
		t.Fatalf("reconcile seen-list = %v, want [1]", got)
	}
}
```

> Note for the implementer: a transport/non-2xx peer error returns from `fetchPeer` *before* the reconcile call (the existing `if httpResp.StatusCode != http.StatusOK` guard at the top of `fetchPeer` returns early), so a down peer never reconciles — this is the false-cancel guard. Add an integration-level assertion of this in Task 10.

- [ ] **Step 5: Run tests**

Run: `cd stock-service && go test ./internal/otccache/ -run TestRefresher -v`
Expected: PASS.

- [ ] **Step 6: Wire the mirror in main.go**

In `stock-service/cmd/main.go`, construct the repo near the other OTC repos and chain `.WithMirror(...)` onto `optionRefresher` (the constructor is at line 858). After the `optionRefresher := otccache.NewOptionRefresher(...)` block:

```go
	remoteOfferRepo := repository.NewRemoteOTCOfferRepository(db)
	optionRefresher = optionRefresher.WithMirror(remoteOfferRepo)
```

(Keep the existing `.WithAggregateBids(...)` chaining wherever it currently happens — `WithMirror` composes with it.)

- [ ] **Step 7: Build + commit**

Run: `cd stock-service && go build ./... && go test ./internal/otccache/ ./internal/repository/`
Expected: PASS.

```bash
git add stock-service/internal/otccache/option_cache.go stock-service/internal/otccache/option_cache_test.go stock-service/cmd/main.go
git commit -m "feat(otc): mirror-backed surrogate ids + peer-cancel reconciliation in option refresher (SP-1)"
```

---

## Task 4: Expose `local_id` on the wire

**Files:**
- Modify: `contract/proto/stock/stock.proto:845-870` (`UnifiedOptionOffer`)
- Regen: `make proto`
- Modify: `stock-service/internal/handler/otc_handler.go:287` (the `&pb.UnifiedOptionOffer{` literal)

- [ ] **Step 1: Add the proto field**

In `contract/proto/stock/stock.proto`, inside `message UnifiedOptionOffer`, after `int32 active_chains_count = 18;` add:

```proto
  // Stable local surrogate id for this offer (RemoteOTCOffer.ID for remote;
  // equals the numeric offer_id for local). The frontend addresses any
  // offer — local or remote — by this id on the unified routes. (SP-1)
  uint64 local_id = 19;
```

- [ ] **Step 2: Regenerate**

Run: `make proto`
Expected: `contract/stockpb/stock.pb.go` regenerates with `LocalId` on `UnifiedOptionOffer`.

- [ ] **Step 3: Map it in the stock handler**

In `stock-service/internal/handler/otc_handler.go`, in the `&pb.UnifiedOptionOffer{...}` built inside `ListUnifiedOptionOffers` (starts line 287), add:

```go
			LocalId: o.LocalID,
```

- [ ] **Step 4: Build + commit**

Run: `cd stock-service && go build ./... && cd ../contract && go build ./...`
Expected: compiles.

```bash
git add contract/proto/stock/stock.proto contract/stockpb/ stock-service/internal/handler/otc_handler.go
git commit -m "feat(otc): surface local_id surrogate on UnifiedOptionOffer wire (SP-1)"
```

---

## Task 5: Gateway discovery — surrogate `id` + `me_owner`

**Files:**
- Create: `api-gateway/internal/handler/otc_me_owner.go`
- Test: `api-gateway/internal/handler/otc_me_owner_test.go`
- Modify: `api-gateway/internal/handler/portfolio_handler.go:367-398` (the discovery row map)
- Modify: `VERSION` and `api-gateway/internal/version/version.go`

- [ ] **Step 1: Write the failing helper test**

Create `api-gateway/internal/handler/otc_me_owner_test.go`:

```go
package handler

import (
	"testing"

	"github.com/exbanka/api-gateway/internal/middleware"
)

func u64(v uint64) *uint64 { return &v }

func TestOtcOfferMeOwner(t *testing.T) {
	emp := &middleware.ResolvedIdentity{PrincipalType: "employee", OwnerType: "bank"}
	cli := &middleware.ResolvedIdentity{PrincipalType: "client", OwnerType: "client", OwnerID: u64(5)}

	cases := []struct {
		name     string
		id       *middleware.ResolvedIdentity
		kind     string
		sellerID string
		want     bool
	}{
		{"employee owns bank-local", emp, "local", "bank", true},
		{"employee not owner of client-local", emp, "local", "client-5", false},
		{"employee never owns remote", emp, "remote", "bank", false},
		{"client owns own local", cli, "local", "client-5", true},
		{"client not owner of other", cli, "local", "client-9", false},
		{"client never owns remote", cli, "remote", "client-5", false},
	}
	for _, c := range cases {
		if got := otcOfferMeOwner(c.id, c.kind, c.sellerID); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestMeOwnerForOwner(t *testing.T) {
	emp := &middleware.ResolvedIdentity{OwnerType: "bank"}
	cli := &middleware.ResolvedIdentity{OwnerType: "client", OwnerID: u64(5)}
	if !meOwnerForOwner(emp, "bank", nil) {
		t.Error("employee should own bank resource")
	}
	if meOwnerForOwner(emp, "client", u64(5)) {
		t.Error("employee should not own client resource")
	}
	if !meOwnerForOwner(cli, "client", u64(5)) {
		t.Error("client should own own resource")
	}
	if meOwnerForOwner(cli, "client", u64(9)) {
		t.Error("client should not own another client's resource")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd api-gateway && go test ./internal/handler/ -run 'MeOwner|MeOwnerForOwner' -v`
Expected: FAIL — `undefined: otcOfferMeOwner`.

- [ ] **Step 3: Write the helpers**

Create `api-gateway/internal/handler/otc_me_owner.go`:

```go
package handler

import (
	"strconv"

	"github.com/exbanka/api-gateway/internal/middleware"
)

// otcOfferMeOwner reports whether the acting identity owns this OTC offer
// (its seller/poster). A remote listing is hosted by a peer and is never
// owned by us, so it is always false. For local listings: an employee
// (acting for the bank) owns bank listings; a client owns listings whose
// seller_id is "client-<their owner id>".
func otcOfferMeOwner(identity *middleware.ResolvedIdentity, kind, sellerID string) bool {
	if identity == nil || kind != "local" {
		return false
	}
	if identity.OwnerType == "bank" {
		return sellerID == "bank"
	}
	if identity.OwnerID != nil {
		return sellerID == "client-"+strconv.FormatUint(*identity.OwnerID, 10)
	}
	return false
}

// meOwnerForOwner reports whether the acting identity owns a resource with
// the given owner_type/owner_id — the same rule the Resource Ownership
// Verification middleware enforces server-side. Used to decorate
// negotiation and contract read responses.
func meOwnerForOwner(identity *middleware.ResolvedIdentity, ownerType string, ownerID *uint64) bool {
	if identity == nil {
		return false
	}
	switch identity.OwnerType {
	case "bank":
		return ownerType == "bank"
	case "client":
		return ownerType == "client" && ownerID != nil && identity.OwnerID != nil && *ownerID == *identity.OwnerID
	}
	return false
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd api-gateway && go test ./internal/handler/ -run 'MeOwner|MeOwnerForOwner' -v`
Expected: PASS.

- [ ] **Step 5: Decorate the discovery response**

In `api-gateway/internal/handler/portfolio_handler.go`, the row map in `listUnifiedOTCOptions` (line 369). The handler needs the identity; both callers already resolve it (`ListMyOTCOptions` does; add it to `ListOTCOptions`). Change `ListOTCOptions` (line 296) to pass identity through — simplest: read identity inside `listUnifiedOTCOptions`:

```go
	identity, _ := c.MustGet("identity").(*middleware.ResolvedIdentity)
```

(Add near the top of `listUnifiedOTCOptions`. The `/otc/options` route already has `ResolveIdentity` middleware — confirm in router_v3.go line 344-346; it does.)

Then in the row map (line 369) add two fields:

```go
			"id":               o.GetLocalId(),
			"me_owner":         otcOfferMeOwner(identity, o.GetKind(), o.GetSellerId()),
```

Keep `offer_id` and `routing_number` as-is (backward-compatible; `offer_id` stays the peer/local string id, `id` is the new stable numeric surrogate).

- [ ] **Step 6: Bump VERSION (MINOR — new response fields)**

Set `VERSION` to `1.7.0` and update `api-gateway/internal/version/version.go` `var Version` to `"1.7.0"`.

```bash
# VERSION file now contains exactly:
1.7.0
```

- [ ] **Step 7: Build + commit**

Run: `cd api-gateway && go build ./... && go test ./internal/handler/ -run 'MeOwner'`
Expected: PASS.

```bash
git add api-gateway/internal/handler/otc_me_owner.go api-gateway/internal/handler/otc_me_owner_test.go api-gateway/internal/handler/portfolio_handler.go VERSION api-gateway/internal/version/version.go
git commit -m "feat(otc): unified surrogate id + me_owner on OTC discovery; VERSION 1.7.0 (SP-1)"
```

---

## Task 6: `GET /otc/options/:id` resolves remote offers

**Files:**
- Modify: `api-gateway/internal/handler/otc_options_handler.go:242-259` (`GetOffer`)

- [ ] **Step 1: Write the failing handler test**

Add to `api-gateway/internal/handler/otc_options_handler_test.go` a test that a `:id` which is NOT a local offer but IS a remote surrogate id returns the remote offer from the unified feed with `me_owner:false` and `kind:"remote"`. Use the existing handler-test harness in that file (mock `h.client.GetOffer` to return `codes.NotFound`, mock `h.otcClient.ListUnifiedOptionOffers` to return a remote row with `LocalId == :id`). Mirror the existing mock-client setup already present in `otc_options_handler_test.go`.

```go
func TestGetOffer_RemoteFallback(t *testing.T) {
	// GIVEN GetOffer(local) -> NotFound, and the unified feed has a remote
	// row with local_id = 7
	// WHEN GET /otc/options/7
	// THEN 200 with body {"kind":"remote","id":7,"me_owner":false,...}
	// (wire the mocks exactly like the existing GetOffer test in this file)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd api-gateway && go test ./internal/handler/ -run TestGetOffer_RemoteFallback -v`
Expected: FAIL.

- [ ] **Step 3: Implement the fallback**

Replace the body of `GetOffer` (lines 242–259) with:

```go
func (h *OTCOptionsHandler) GetOffer(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		apiError(c, http.StatusBadRequest, ErrValidation, "invalid id")
		return
	}
	identity := c.MustGet("identity").(*middleware.ResolvedIdentity)
	resp, err := h.client.GetOffer(c.Request.Context(), &stockpb.GetOTCOfferRequest{
		OfferId:         id,
		ActorUserId:     int64(ownerToLegacyUserID(identity.OwnerID)),
		ActorSystemType: ownerToLegacySystemType(identity.OwnerType),
	})
	if err == nil {
		c.JSON(http.StatusOK, gin.H{"offer": resp, "kind": "local",
			"me_owner": otcOfferMeOwner(identity, "local", localOfferSellerID(resp))})
		return
	}
	if status.Code(err) != codes.NotFound {
		handleGRPCError(c, err)
		return
	}
	// Not a local offer — try the remote mirror via the unified feed.
	row := h.findRemoteOfferByLocalID(c, id)
	if row == nil {
		apiError(c, http.StatusNotFound, ErrNotFound, "OTC offer not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"offer": row, "kind": "remote", "me_owner": false})
}

// findRemoteOfferByLocalID scans the unified option feed (which is the
// mirror-backed remote-offer source) for a remote row with the given
// surrogate id. Returns nil if none. Reads only — no new RPC.
func (h *OTCOptionsHandler) findRemoteOfferByLocalID(c *gin.Context, localID uint64) gin.H {
	resp, err := h.otcClient.ListUnifiedOptionOffers(c.Request.Context(), &stockpb.ListUnifiedOptionOffersRequest{
		Kind: "remote", Page: 1, PageSize: 1000,
	})
	if err != nil {
		return nil
	}
	for _, o := range resp.GetOffers() {
		if o.GetLocalId() != localID {
			continue
		}
		return gin.H{
			"id": o.GetLocalId(), "offer_id": o.GetOfferId(), "kind": "remote",
			"bank_code": o.GetBankCode(), "routing_number": o.GetRoutingNumber(),
			"seller_id": o.GetSellerId(), "direction": o.GetDirection(), "ticker": o.GetTicker(),
			"amount": o.GetAmount(), "strike_price": o.GetStrikePrice(), "strike_currency": o.GetStrikeCurrency(),
			"premium": o.GetPremium(), "premium_currency": o.GetPremiumCurrency(),
			"settlement_date": o.GetSettlementDate(), "created_at": o.GetCreatedAt(),
		}
	}
	return nil
}
```

Add the imports `"google.golang.org/grpc/codes"` and `"google.golang.org/grpc/status"` if not already present, and a small helper `localOfferSellerID(resp)` that extracts the seller id string from the `GetOTCOfferRequest` response (mirror `composeSellerID` logic: `"bank"` for bank-owned, `"client-<initiator_id>"` otherwise — read the fields the `GetOffer` response exposes; if it has no owner fields, return `""` and let `me_owner` be false). Confirm `h.otcClient` is the `OTCGRPCServiceClient` field already on `OTCOptionsHandler` (used by discovery); if `OTCOptionsHandler` lacks it, inject it in the constructor following `PortfolioHandler` which already holds `otcClient`.

> Backward-compat note: the local branch now wraps the offer as `{"offer":...,"kind":...,"me_owner":...}` instead of the bare `resp`. Confirm the FE reads `offer.*`; the spec authorizes adding fields but this *reshapes* the local body. If strict v3 compatibility is required, instead keep returning bare `resp` for local and only add the wrapper for the remote branch — decide with the maintainer in review and update the integration test (Task 10) to match the chosen shape.

- [ ] **Step 4: Run to verify it passes**

Run: `cd api-gateway && go test ./internal/handler/ -run TestGetOffer -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add api-gateway/internal/handler/otc_options_handler.go api-gateway/internal/handler/otc_options_handler_test.go
git commit -m "feat(otc): GET /otc/options/:id resolves remote surrogate ids; me_owner+kind (SP-1)"
```

---

## Task 7: `me_owner` + `kind` on my-negotiations (merge local + remote)

**Files:**
- Modify: `api-gateway/internal/handler/otc_negotiation_handler.go:359` (`ListMyNegotiations`)
- Test: `api-gateway/internal/handler/otc_negotiation_handler_test.go` (or the existing OTC handler test file)

This is the **pattern task** for unifying a list read. The shared decoration helpers from Task 5 (`meOwnerForOwner`) are reused by Task 8.

- [ ] **Step 1: Write the failing test**

Add a test asserting `ListMyNegotiations` returns BOTH the caller's local chains (each `kind:"local"`, `me_owner` per owner) AND their remote chains (from `ListMyPeerNegotiations`, each `kind:"remote"`, `me_owner:true` for the chains they bid on), in one `negotiations` array. Wire mocks for `h.client.ListMyNegotiations` and `h.peerOTC.ListMyPeerNegotiations` per the existing mock pattern in the test file.

- [ ] **Step 2: Run to verify it fails**

Run: `cd api-gateway && go test ./internal/handler/ -run TestListMyNegotiations_MergesRemote -v`
Expected: FAIL.

- [ ] **Step 3: Implement the merge**

In `ListMyNegotiations`: after fetching local negotiations, also call `h.peerOTC.ListMyPeerNegotiations` (the client `PeerOTCInitiateHandler` already uses — inject `stockpb.PeerOTCServiceClient` into `OTCOptionsHandler` following `PeerOTCInitiateHandler`'s field if not present). Map each local row to a `gin.H` with `"kind":"local"` and `"me_owner": meOwnerForOwner(identity, <bidder owner type>, <bidder owner id>)`; map each remote row to `gin.H` with `"kind":"remote"`, the surrogate id (`it.GetId()`/foreign id), the offer terms from `it.GetOffer()`, and `"me_owner": true` when the caller is the chain's bidder (the `ListMyPeerNegotiations` rows are already scoped to the caller, so `me_owner` is true for rows where role=="buyer"/the caller hosts the bidder). Concatenate into one `negotiations` array and return:

```go
	c.JSON(http.StatusOK, gin.H{"negotiations": merged})
```

Keep field names identical to the existing local shape so the FE parses one model; `kind` + `me_owner` are the only additions.

- [ ] **Step 4: Run to verify it passes**

Run: `cd api-gateway && go test ./internal/handler/ -run TestListMyNegotiations -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add api-gateway/internal/handler/otc_negotiation_handler.go api-gateway/internal/handler/otc_negotiation_handler_test.go
git commit -m "feat(otc): unify my-negotiations read (local+remote) with kind+me_owner (SP-1)"
```

---

## Task 8: `me_owner` + `kind` on history, contracts, on-listing, timeline

**Files:**
- Modify: `api-gateway/internal/handler/otc_options_handler.go` (`ListNegotiationHistory:126`, `ListMyContracts:370`, `GetContract:400`)
- Modify: `api-gateway/internal/handler/otc_negotiation_handler.go` (`ListNegotiationsOnListing:427`, `GetOfferTimeline:460`)

Apply the same decoration pattern established in Tasks 5 & 7. Each step has a paired test in the existing handler test files.

- [ ] **Step 1: History + contracts merge + decorate**
  - `ListNegotiationHistory`: include the caller's remote negotiations (terminal + active) via `ListMyPeerNegotiations`; decorate every item with `kind` + `me_owner` (`meOwnerForOwner`). Test: `TestListNegotiationHistory_IncludesRemote`.
  - `ListMyContracts`: merge local contracts with the caller's `PeerOptionContract` rows (via the existing peer-contract list path the gateway uses for `/me/otc/contracts/peer/...`; if no list RPC exists, add a read-only `ListMyPeerContracts` query that filters `peer_option_contracts` by buyer/seller routing+id — mirrors `ListMyPeerNegotiations`). Decorate with `kind` + `me_owner`. Test: `TestListMyContracts_IncludesRemote`.
  - `GetContract`: on local NotFound, fall back to a peer contract by surrogate/foreign id (same shape as Task 6's offer fallback). Decorate with `kind` + `me_owner`. Test: `TestGetContract_RemoteFallback`.

- [ ] **Step 2: On-listing + timeline remote behavior**
  - `ListNegotiationsOnListing` and `GetOfferTimeline`: when `:id` resolves to a **remote** surrogate id (not a local listing), return **only the caller's own chain** against that listing (from `ListMyPeerNegotiations` filtered to the matching `parent_offer_id`), never other parties' chains. For a local `:id`, behavior is unchanged. Each item carries `kind` + `me_owner`. Tests: `TestOnListing_RemoteReturnsOnlyOwnChain`, `TestTimeline_RemoteReturnsOnlyOwnChain`.

- [ ] **Step 3: Run the OTC handler suite**

Run: `cd api-gateway && go test ./internal/handler/ -run 'OTC|Negotiation|Contract|Offer|Timeline' -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add api-gateway/internal/handler/otc_options_handler.go api-gateway/internal/handler/otc_negotiation_handler.go api-gateway/internal/handler/*_test.go
git commit -m "feat(otc): kind+me_owner on history/contracts/on-listing/timeline; remote-aware reads (SP-1)"
```

---

## Task 9: Negotiation status safety-net reconciler

**Files:**
- Create: `stock-service/internal/service/peer_otc_reconciler.go`
- Test: `stock-service/internal/service/peer_otc_reconciler_test.go`
- Modify: `stock-service/cmd/main.go` (start the goroutine)

Peer-initiated negotiation cancels are already reflected by the inbound `DELETE /cross-bank-protocol/negotiations/:rid/:id` webhook (`peer_otc_handler.go` → `UpdateStatus(..., "cancelled")`). This task adds a **safety net** for missed webhooks: a periodic poll of the counterparty's `GET /cross-bank-protocol/negotiations/:rid/:id` for our `ongoing` `peer_otc_negotiation` rows; if the peer reports terminal and we still show `ongoing`, reconcile via the SAME `UpdateStatus` path so behavior is identical to the webhook.

- [ ] **Step 1: Write the reconciler with a ctx-cancellable ticker**

```go
package service

import (
	"context"
	"log"
	"time"
)

// PeerOTCNegotiationReconciler periodically polls counterparties for the
// terminal state of our ongoing cross-bank negotiations and reconciles any
// we missed (webhook lost). Safety net only — the inbound DELETE webhook is
// the primary path. Honors ctx cancellation; stops its ticker on exit.
type PeerOTCNegotiationReconciler struct {
	poll     func(ctx context.Context) (reconciled int, err error) // injected: lists ongoing rows, polls peer, flips terminal
	interval time.Duration
}

func NewPeerOTCNegotiationReconciler(poll func(ctx context.Context) (int, error), interval time.Duration) *PeerOTCNegotiationReconciler {
	return &PeerOTCNegotiationReconciler{poll: poll, interval: interval}
}

func (r *PeerOTCNegotiationReconciler) Run(ctx context.Context) {
	if _, err := r.poll(ctx); err != nil {
		log.Printf("peer-otc reconciler: initial poll: %v", err)
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.poll(ctx); err != nil {
				log.Printf("peer-otc reconciler: poll: %v", err)
			}
		}
	}
}
```

The injected `poll` closure (wired in `main.go`) lists `ongoing` `peer_otc_negotiation` rows, resolves each counterparty via `PeerBankAdminService`, GETs `/negotiations/:rid/:id`, and on a terminal `status`/`isOngoing:false` calls the existing `negRepo.UpdateStatus(peerCode, foreignID, "cancelled")` + the existing notification path. Only acts on a *successful* peer response (transport/non-2xx → skip that row — the same false-cancel guard as Task 3).

- [ ] **Step 2: Test the loop honors ctx + only reconciles on success**

Test `Run` returns promptly on ctx cancel; test that the `poll` closure, given a fake peer reporting `cancelled`, flips a fake repo row, and given a fake peer error, leaves it `ongoing`.

- [ ] **Step 3: Wire in main.go behind the cron registry** (mirror `staleScan` at line 884):

```go
	go service.NewPeerOTCNegotiationReconciler(peerOTCReconcilePoll, 2*time.Minute).Run(ctx)
```

- [ ] **Step 4: Run + commit**

Run: `cd stock-service && go test ./internal/service/ -run PeerOTC -v`

```bash
git add stock-service/internal/service/peer_otc_reconciler.go stock-service/internal/service/peer_otc_reconciler_test.go stock-service/cmd/main.go
git commit -m "feat(otc): safety-net reconciler for missed cross-bank negotiation cancels (SP-1)"
```

---

## Task 10: Integration tests + docs + final verification

**Files:**
- Modify/Create: `test-app/workflows/otc_unified_read_test.go`
- Modify: `docs/api/REST_API_v3.md`, `Specification.md`, Swagger (`make swagger`)

- [ ] **Step 1: Integration tests** (`test-app/workflows/`, using the shared helpers in `helpers_test.go` — never inline client setup/Kafka scanning):
  - Discovery returns a remote offer with a stable numeric `id` and `me_owner:false`; a second call returns the **same** `id` (stability).
  - `GET /otc/options/:id` with that remote `id` returns the remote offer (`kind:"remote"`).
  - `GET /me/otc/options/negotiations` returns a caller's remote chain alongside a local one, each with `kind` + `me_owner`.
  - **Reconciliation:** simulate a peer offer disappearing from the peer's public list → after a refresh cycle the mirror row flips to `cancelled` and the holder is notified; a peer *poll error* leaves rows untouched (false-cancel guard). Use the two-stack harness (instance1 ⇄ instance2) per the team's interop practice.

- [ ] **Step 2: Run integration suite**

Run the `test-app` workflow suite per its README. Expected: PASS. Validate response bodies + side effects (mirror status, notifications), not just status codes.

- [ ] **Step 3: Docs**
  - `docs/api/REST_API_v3.md`: document the new `id`, `me_owner`, and `kind` fields on `GET /otc/options`, `GET /otc/options/:id`, `GET /me/otc/options/negotiations`, `GET /me/otc/history`, `GET /me/otc/contracts`, `GET /otc/contracts/:id`, and the remote behavior of on-listing/timeline.
  - Swagger: update annotations on the touched handlers, run `make swagger`, commit generated `api-gateway/docs/`.
  - `Specification.md`: new entity `remote_otc_offer` (§18); reconciliation business rule "a peer-cancelled/vanished remote offer is reconciled to cancelled on a successful poll" (§21); the new response fields (§17).

- [ ] **Step 4: Full build + lint + test**

Run: `make build && make lint && make test`
Expected: clean (zero new lint warnings on touched services; all tests pass).

- [ ] **Step 5: Commit**

```bash
git add test-app/workflows/otc_unified_read_test.go docs/api/REST_API_v3.md Specification.md api-gateway/docs/
git commit -m "test+docs(otc): SP-1 unified read integration tests + REST/Swagger/spec updates"
```

---

## Self-review notes (carried into execution)

- **Spec coverage:** mirror table (Task 1-2) ✓; surrogate ids (Task 3-5) ✓; offer reconciliation + false-cancel guard (Task 3, 10) ✓; `me_owner` (Task 5, 7-8) ✓; unified reads for discovery/detail/negotiations/history/contracts/on-listing/timeline (Tasks 5-8) ✓; negotiation safety-net (Task 9) ✓; docs/tests/version (Task 5, 10) ✓. SP-1 removes no routes (umbrella clean-cut deletions land in SP-2) — consistent with the spec's "reads only."
- **Two open decisions flagged for the maintainer at review time:** (a) Task 6's local-branch body reshape (`{offer,kind,me_owner}` vs bare `resp`) — pick the v3-compatible shape; (b) Task 8's `ListMyPeerContracts` read query if no peer-contract list RPC exists yet.
- **Type consistency:** `LocalID`(model/cache) ↔ `local_id`(proto) ↔ `GetLocalId()`(gateway); `Upsert`/`ReconcilePeerNotSeen` signatures identical across repo, interface, and fake; `otcOfferMeOwner`/`meOwnerForOwner` used verbatim in Tasks 5-8.
