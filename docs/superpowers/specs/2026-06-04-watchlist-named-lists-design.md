# Watchlist — Multiple Named Lists (SP6 / TODO_final item G, Celina 3)

**Date:** 2026-06-04 · All in `stock-service` + gateway.

## Requirement
Let clients/actuaries keep **multiple named watchlists** (e.g. "tech stocks", "forex pairs"), not one implicit list. Add named-list CRUD + per-list items; keep the existing single-list endpoints working (backward compatible).

## Existing (from exploration)
- `WatchlistItem{owner_type, owner_id, listing_id}` with unique `(owner_type, owner_id, listing_id)`. Repo `Add/Remove/ListWithListings/ListAllClientWatchlistItems/Exists`. Service `Add/Remove/List`. Proto `WatchlistService.AddItem/RemoveItem/ListMy`. Gateway `/api/v3/me/watchlist` (GET/POST/DELETE :listing_id). The daily `WatchlistNotificationCron` scans `ListAllClientWatchlistItems("")` (per-owner, dedup per owner+ticker+day).

## Design (least-disruptive: keep `owner` on the item → cron unchanged)
- **New model `Watchlist`** (parent): `id, owner_type, owner_id, name, created_at, updated_at`; unique `(owner_type, owner_id, name)`.
- **`WatchlistItem` gains `WatchlistID uint64`** (FK). Unique index becomes `(watchlist_id, listing_id)` (the same listing may live in different named lists). **Keep `owner_type`/`owner_id` on the item** (denormalized from the parent) so the notification cron's per-owner scan and dedup are unchanged.
- **Lazy default list:** `getOrCreateDefaultWatchlist(owner)` returns the owner's "My Watchlist", creating it on first use. The legacy `/me/watchlist` add/list/remove operate on this default list, so existing clients keep working.
- **Startup migration** (`watchlist_cutover.go`, idempotent, mirrors the SP1 tax cutover): (1) drop the old unique index `idx_watchlist_owner_listing` if present; (2) for every distinct owner with items whose `watchlist_id = 0`, get-or-create their default list and set those items' `watchlist_id`.

### Repository (`watchlist_repository.go`)
- New: `CreateWatchlist(*Watchlist) error` (ON CONFLICT on name → return existing/err), `GetWatchlist(id) (*Watchlist, error)`, `ListWatchlists(owner) ([]Watchlist, error)`, `DeleteWatchlist(id) (bool, error)` (cascade-delete its items), `GetOrCreateDefault(owner) (*Watchlist, error)`.
- Modify: `Add(item)` now requires `item.WatchlistID`; unique now `(watchlist_id, listing_id)`. `Remove(watchlistID, listingID)`. `ListWithListingsByWatchlist(watchlistID, listingType)`. `ListAllClientWatchlistItems` unchanged (still per-owner).

### Service (`watchlist_service.go`)
- New: `CreateWatchlist(owner, name) (Watchlist, error)` (validate non-empty, length); `ListWatchlists(owner) ([]Watchlist, error)`; `DeleteWatchlist(owner, watchlistID) error` (ownership-checked; cannot delete if not owner; deleting the default is allowed and it re-creates lazily).
- Modify: `AddToList(owner, watchlistID, listingID)` / `RemoveFromList(owner, watchlistID, listingID)` / `ListByWatchlist(owner, watchlistID, listingType)` — each verifies the list belongs to the owner. Default-list wrappers `Add/Remove/List(owner, listingType)` resolve the default list then delegate (legacy path).

### Proto (`stock.proto WatchlistService`)
- New RPCs: `CreateWatchlist`, `ListWatchlists`, `DeleteWatchlist`, `ListWatchlistItems`. Messages: `Watchlist{id, name, item_count, created_at}`, requests carry `owner_type/owner_id` + (for item/list ops) `watchlist_id`.
- Existing `AddItem`/`RemoveItem` gain an optional `watchlist_id` (0 → default list). `ListMy` keeps returning the default list (legacy); `ListWatchlistItems` returns a specific list.

### Gateway (`watchlist_handler.go`, `router_v3.go`)
- New routes (under `/api/v3/me`, `bankIfEmp`):
  - `GET /watchlists` → list named lists; `POST /watchlists` `{name}` → create; `DELETE /watchlists/:watchlist_id` → delete.
  - `GET /watchlists/:watchlist_id/items?listing_type=` → items; `POST /watchlists/:watchlist_id/items` `{listing_id}` → add; `DELETE /watchlists/:watchlist_id/items/:listing_id` → remove.
- Keep legacy `GET/POST/DELETE /watchlist[/:listing_id]` operating on the default list (unchanged behavior).
- Ownership: the list's owner must match the resolved identity (verified service-side via owner match; gateway passes resolved owner).

## Concurrency / safety
- `getOrCreateDefault` and `CreateWatchlist` use `ON CONFLICT (owner_type, owner_id, name) DO NOTHING` + re-read, so concurrent first-use is race-safe (no duplicate default).
- Item `Add` stays idempotent (`ON CONFLICT (watchlist_id, listing_id) DO NOTHING`).
- `DeleteWatchlist` removes the parent + its items in one transaction.
- Startup migration is idempotent (drop-if-exists + only touches `watchlist_id=0` items).

## Testing
- **Repo unit:** create/list/delete named lists; item add scoped to a list; same listing in two lists allowed; `GetOrCreateDefault` idempotent.
- **Service unit:** ownership enforcement (can't add to / delete another owner's list); default-list wrappers resolve+delegate; create validates name.
- **Cron unit:** existing tests still pass (item still carries owner) — no change.
- **Integration:** create two named lists; add a listing to each; list them; legacy `/me/watchlist` still works against the default list; delete a list.

## Docs / version
- `Specification.md` (entity + routes), `docs/api/REST_API_v3.md` (new routes), swagger. VERSION MINOR bump.
