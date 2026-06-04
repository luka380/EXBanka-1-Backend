package service

import (
	"log"

	"gorm.io/gorm"

	"github.com/exbanka/stock-service/internal/model"
	"github.com/exbanka/stock-service/internal/repository"
)

// MigrateWatchlistsToNamedLists is a one-time, idempotent startup migration for
// SP6: it drops the legacy per-(owner, listing) unique index and assigns every
// pre-existing watchlist item (watchlist_id = 0) to its owner's default
// "My Watchlist", creating that list lazily. Safe to run on every startup —
// once all items have a watchlist_id, the backfill loop finds nothing.
func MigrateWatchlistsToNamedLists(db *gorm.DB, repo *repository.WatchlistRepository) error {
	// Drop the old unique index so the same listing may live in multiple named
	// lists (the new unique index is on (watchlist_id, listing_id)).
	if err := db.Exec("DROP INDEX IF EXISTS idx_watchlist_owner_listing").Error; err != nil {
		log.Printf("WARN: watchlist migration: drop legacy index: %v", err)
	}

	type ownerRow struct {
		OwnerType string
		OwnerID   *uint64
	}
	var owners []ownerRow
	if err := db.Model(&model.WatchlistItem{}).
		Where("watchlist_id = 0 OR watchlist_id IS NULL").
		Distinct("owner_type", "owner_id").
		Find(&owners).Error; err != nil {
		return err
	}
	for _, o := range owners {
		def, err := repo.GetOrCreateDefault(model.OwnerType(o.OwnerType), o.OwnerID)
		if err != nil {
			log.Printf("WARN: watchlist migration: default list for %s/%v: %v", o.OwnerType, o.OwnerID, err)
			continue
		}
		// SkipHooks: a bulk Update builds a zero-value WatchlistItem whose
		// BeforeSave would reject the empty owner_type (CLAUDE.md bulk-update
		// gotcha). We're only setting watchlist_id, so the owner validation is
		// irrelevant here.
		q := db.Session(&gorm.Session{SkipHooks: true}).Model(&model.WatchlistItem{}).
			Where("owner_type = ? AND (watchlist_id = 0 OR watchlist_id IS NULL)", o.OwnerType)
		if o.OwnerID == nil {
			q = q.Where("owner_id IS NULL")
		} else {
			q = q.Where("owner_id = ?", *o.OwnerID)
		}
		if err := q.Update("watchlist_id", def.ID).Error; err != nil {
			log.Printf("WARN: watchlist migration: backfill items for %s/%v: %v", o.OwnerType, o.OwnerID, err)
		}
	}
	return nil
}
