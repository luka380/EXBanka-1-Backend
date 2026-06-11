package handler

import (
	"context"
	"log"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/exbanka/contract/stockpb"
	"github.com/exbanka/stock-service/internal/model"
	"github.com/exbanka/stock-service/internal/otccache"
	"github.com/exbanka/stock-service/internal/service"
)

// otcSvcFacade is the narrow interface of OTCService used by OTCHandler.
type otcSvcFacade interface {
	ListOffers(filter service.OTCFilter) ([]model.Holding, int64, error)
	BuyOffer(offerID, buyerID uint64, buyerSystemType string, quantity int64, buyerAccountID uint64) (*service.OTCBuyResult, error)
}

type OTCHandler struct {
	pb.UnimplementedOTCGRPCServiceServer
	otcSvc      otcSvcFacade
	cache       *otccache.Cache
	optionCache *otccache.OptionCache // optional; Phase 6 cross-bank option discovery
	// myNegs is optional; when wired, ListUnifiedOptionOffers stamps
	// my_negotiation_id / my_negotiation_status on each offer the
	// authenticated caller has an own (bidder) chain against (SP-2b).
	// ownRouting keys the remote-chain → remote-offer match.
	myNegs     MyNegotiationLister
	ownRouting int64
	// ownerLatestCounter is optional; when wired, ListUnifiedOptionOffers
	// re-sources the strike/premium/settlement of each LOCAL offer the caller
	// OWNS from the acting principal's most recent counter on that offer (D2).
	ownerLatestCounter OwnerLatestCounterFn
}

// NewOTCHandler keeps the prior signature for backwards compatibility
// with cmd/main.go callers that don't yet pass a cache. Pass a nil
// cache to disable the unified-offers RPC (it will return an empty
// list with peers_total=0).
func NewOTCHandler(otcSvc *service.OTCService) *OTCHandler {
	return &OTCHandler{otcSvc: otcSvc}
}

// NewOTCHandlerWithCache wires the unified-offers cache. cmd/main.go
// uses this once the cache + refresher are constructed.
func NewOTCHandlerWithCache(otcSvc *service.OTCService, cache *otccache.Cache) *OTCHandler {
	return &OTCHandler{otcSvc: otcSvc, cache: cache}
}

// WithOptionCache wires the Phase-6 cross-bank option-discovery cache
// (parallel to the stocks-marketplace cache). Returns a copy so cmd/
// main.go can chain wire-up calls.
func (h *OTCHandler) WithOptionCache(c *otccache.OptionCache) *OTCHandler {
	cp := *h
	cp.optionCache = c
	return &cp
}

// WithMyNegotiations wires the caller's own (bidder) negotiation source so
// ListUnifiedOptionOffers can stamp my_negotiation_id / my_negotiation_status
// on each offer the authenticated caller is negotiating (SP-2b). ownRouting
// keys the remote-chain → remote-offer match. Returns a copy so cmd/main.go can
// chain wire-up calls.
func (h *OTCHandler) WithMyNegotiations(l MyNegotiationLister, ownRouting int64) *OTCHandler {
	cp := *h
	cp.myNegs = l
	cp.ownRouting = ownRouting
	return &cp
}

// WithOwnerLatestCounter wires the owner-latest-counter source so
// ListUnifiedOptionOffers can project the acting owner's most recent counter
// terms onto the LOCAL offers they own (D2). Returns a copy so cmd/main.go can
// chain wire-up calls.
func (h *OTCHandler) WithOwnerLatestCounter(fn OwnerLatestCounterFn) *OTCHandler {
	cp := *h
	cp.ownerLatestCounter = fn
	return &cp
}

func newOTCHandlerForTest(otcSvc otcSvcFacade) *OTCHandler {
	return &OTCHandler{otcSvc: otcSvc}
}

func (h *OTCHandler) ListOffers(ctx context.Context, req *pb.ListOTCOffersRequest) (*pb.ListOTCOffersResponse, error) {
	filter := service.OTCFilter{
		SecurityType: req.SecurityType,
		Ticker:       req.Ticker,
		Page:         int(req.Page),
		PageSize:     int(req.PageSize),
	}

	holdings, total, err := h.otcSvc.ListOffers(filter)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	offers := make([]*pb.OTCOffer, len(holdings))
	for i, hld := range holdings {
		offers[i] = &pb.OTCOffer{
			Id:           hld.ID,
			SellerId:     model.OwnerIDOrZero(hld.OwnerID),
			SellerName:   hld.UserFirstName + " " + hld.UserLastName,
			SecurityType: hld.SecurityType,
			Ticker:       hld.Ticker,
			Name:         hld.Name,
			Quantity:     hld.PublicQuantity,
			PricePerUnit: hld.AveragePrice.StringFixed(2),
			CreatedAt:    hld.CreatedAt.Format("2006-01-02T15:04:05Z"),
		}
	}

	return &pb.ListOTCOffersResponse{
		Offers:     offers,
		TotalCount: total,
	}, nil
}

func (h *OTCHandler) BuyOffer(ctx context.Context, req *pb.BuyOTCOfferRequest) (*pb.OTCTransaction, error) {
	// Both the buyer ID and system_type must flip to the client when an
	// employee acts on behalf, so the resulting holding lands in the
	// client's portfolio. Same rule as order placement; see resolveOrderOwner.
	effectiveBuyerID, effectiveSystemType, rerr := resolveOrderOwner(req.BuyerId, req.SystemType, req.ActingEmployeeId, req.OnBehalfOfClientId)
	if rerr != nil {
		return nil, rerr
	}

	result, err := h.otcSvc.BuyOffer(
		req.OfferId,
		effectiveBuyerID,
		effectiveSystemType,
		req.Quantity,
		req.AccountId,
	)
	if err != nil {
		return nil, mapOTCError(err)
	}

	return &pb.OTCTransaction{
		Id:           result.ID,
		OfferId:      result.OfferID,
		Quantity:     result.Quantity,
		PricePerUnit: result.PricePerUnit.StringFixed(2),
		TotalPrice:   result.TotalPrice.StringFixed(2),
		Commission:   result.Commission.StringFixed(2),
	}, nil
}

// mapOTCError is now a passthrough. Service-layer sentinels carry their own
// gRPC code via svcerr.SentinelError. See internal/service/errors.go for the
// OTC sentinel set.
func mapOTCError(err error) error {
	return err
}

// ListUnifiedOffers serves the cached union of local + cross-bank OTC
// offers. Filters and pagination are applied in-memory over the cached
// snapshot. peers_total / peers_reached / partial reflect the most
// recent refresh cycle so the UI can surface "showing 1 of 2 peers".
func (h *OTCHandler) ListUnifiedOffers(ctx context.Context, req *pb.ListUnifiedOTCOffersRequest) (*pb.ListUnifiedOTCOffersResponse, error) {
	page := int(req.GetPage())
	if page < 1 {
		page = 1
	}
	pageSize := int(req.GetPageSize())
	if pageSize < 1 {
		pageSize = 10
	}
	secType := req.GetSecurityType()
	if secType != "" && secType != "stock" && secType != "futures" {
		return nil, status.Error(codes.InvalidArgument, "security_type must be 'stock' or 'futures'")
	}
	kind := req.GetKind()
	if kind != "" && kind != "local" && kind != "remote" {
		return nil, status.Error(codes.InvalidArgument, "kind must be 'local' or 'remote'")
	}
	ticker := strings.ToUpper(req.GetTicker())
	bankFilter := req.GetBankCode()

	if h.cache == nil {
		return &pb.ListUnifiedOTCOffersResponse{}, nil
	}
	snap := h.cache.Get()

	filtered := make([]otccache.Offer, 0, len(snap.Offers))
	for _, o := range snap.Offers {
		if secType != "" && o.SecurityType != secType {
			continue
		}
		if ticker != "" && strings.ToUpper(o.Ticker) != ticker {
			continue
		}
		if kind != "" && o.Kind != kind {
			continue
		}
		if bankFilter != "" && o.BankCode != bankFilter {
			continue
		}
		filtered = append(filtered, o)
	}

	total := int64(len(filtered))
	start := (page - 1) * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}

	out := make([]*pb.UnifiedOTCOffer, 0, end-start)
	for _, o := range filtered[start:end] {
		out = append(out, &pb.UnifiedOTCOffer{
			Kind:         o.Kind,
			BankCode:     o.BankCode,
			Id:           o.ID,
			SellerId:     o.SellerID,
			SellerName:   o.SellerName,
			Name:         o.Name,
			CreatedAt:    o.CreatedAt,
			OwnerId:      o.OwnerID,
			SecurityType: o.SecurityType,
			Ticker:       o.Ticker,
			Quantity:     o.Quantity,
			PricePerUnit: o.PricePerUnit,
			Currency:     o.Currency,
		})
	}

	var lastRefreshUnix int64
	if !snap.LastRefresh.IsZero() {
		lastRefreshUnix = snap.LastRefresh.Unix()
	}
	// Suppress unused-var lint when ctx isn't used; ctx is reserved for
	// future cancellation in case we add per-request peer fan-out.
	_ = ctx
	return &pb.ListUnifiedOTCOffersResponse{
		Offers:          out,
		TotalCount:      total,
		PeersTotal:      int32(snap.PeersTotal),
		PeersReached:    int32(snap.PeersReached),
		Partial:         snap.PeersTotal > 0 && snap.PeersReached < snap.PeersTotal,
		LastRefreshUnix: lastRefreshUnix,
	}, nil
}

// _ explicit static check that OTCHandler still implements every
// generated server interface method (caught at compile time).
var _ pb.OTCGRPCServiceServer = (*OTCHandler)(nil)

// ListUnifiedOptionOffers serves the Phase-6 cross-bank discovery view
// of open OTC OPTION listings. Backed by OptionCache (refreshed every
// ~5 s by OptionRefresher). Filters by ticker, kind (local|remote),
// bank_code, and direction (sell_initiated|buy_initiated); paginates
// in-memory over the cached snapshot. partial=true reflects the most
// recent refresh missing one or more peers.
func (h *OTCHandler) ListUnifiedOptionOffers(ctx context.Context, req *pb.ListUnifiedOptionOffersRequest) (*pb.ListUnifiedOptionOffersResponse, error) {
	page := int(req.GetPage())
	if page < 1 {
		page = 1
	}
	pageSize := int(req.GetPageSize())
	if pageSize < 1 {
		pageSize = 10
	}
	kind := req.GetKind()
	if kind != "" && kind != "local" && kind != "remote" {
		return nil, status.Error(codes.InvalidArgument, "kind must be 'local' or 'remote'")
	}
	direction := req.GetDirection()
	if direction != "" && direction != "sell_initiated" && direction != "buy_initiated" {
		return nil, status.Error(codes.InvalidArgument, "direction must be 'sell_initiated' or 'buy_initiated'")
	}
	if h.optionCache == nil {
		return &pb.ListUnifiedOptionOffersResponse{}, nil
	}
	snap := h.optionCache.Get()
	ticker := strings.ToUpper(req.GetTicker())
	bankFilter := req.GetBankCode()

	ownerOnly := req.GetOwnerOnlySellerId()
	filtered := make([]otccache.OptionOffer, 0, len(snap.Offers))
	for _, o := range snap.Offers {
		if ticker != "" && strings.ToUpper(o.Ticker) != ticker {
			continue
		}
		if kind != "" && o.Kind != kind {
			continue
		}
		if bankFilter != "" && o.BankCode != bankFilter {
			continue
		}
		if direction != "" && o.Direction != direction {
			continue
		}
		if ownerOnly != "" {
			// Owner-scoped: cross-bank listings are never ours, and the
			// SellerID must match exactly (SI-TX form).
			if o.Kind != "local" || o.SellerID != ownerOnly {
				continue
			}
		}
		filtered = append(filtered, o)
	}
	total := int64(len(filtered))
	start := (page - 1) * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	actingOwnerType := req.GetActingOwnerType()
	actingOwnerID := req.GetActingOwnerId()
	// SP-2b — stamp the caller's own (bidder) chain per offer so the FE can
	// jump straight to its chain. Built once over the caller's chains; absent
	// (0 / "") for offers the caller has no chain on. A nil source contributes
	// no stamps. Index errors are best-effort: log and continue without stamps
	// rather than failing the discovery read.
	myNegIdx, err := buildMyNegotiationIndex(h.myNegs, actingOwnerType, actingOwnerID, h.ownRouting)
	if err != nil {
		log.Printf("WARN ListUnifiedOptionOffers: my-negotiation index failed (continuing without my_negotiation_id): %v", err)
		myNegIdx = myNegotiationIndex{}
	}
	// Acting PRINCIPAL ("client"/"employee" + id) for the OWNER term branch. It
	// differs from the acting OWNER (which collapses every employee to "bank"):
	// the revision author of a bank-owned counter is "employee-<N>".
	actorPrincipalType := req.GetActorSystemType()
	actorPrincipalID := uint64(req.GetActorUserId())
	out := make([]*pb.UnifiedOptionOffer, 0, end-start)
	for _, o := range filtered[start:end] {
		meOwner := otcMeOwner(actingOwnerType, actingOwnerID, o.Kind, o.SellerID)
		item := &pb.UnifiedOptionOffer{
			Kind:              o.Kind,
			BankCode:          o.BankCode,
			RoutingNumber:     o.RoutingNumber,
			OfferId:           o.OfferID,
			SellerId:          o.SellerID,
			SellerName:        o.SellerName,
			Direction:         o.Direction,
			Ticker:            o.Ticker,
			Amount:            o.Amount,
			StrikePrice:       o.StrikePrice,
			StrikeCurrency:    o.StrikeCurrency,
			Premium:           o.Premium,
			PremiumCurrency:   o.PremiumCurrency,
			SettlementDate:    o.SettlementDate,
			CreatedAt:         o.CreatedAt,
			BestBid:           o.BestBid,
			BestAsk:           o.BestAsk,
			ActiveChainsCount: o.ActiveChainsCount,
			LocalId:           o.LocalID,
			MeOwner:           meOwner,
		}
		// LOCAL offers: chain keyed by parent_offer_id == local offer id
		// (== LocalID). REMOTE offers: chain keyed by the peer-hosted parent
		// (routing, native) — the remote cache row carries native id in OfferID
		// and the peer routing in RoutingNumber.
		var stamp myNegStamp
		var haveStamp bool
		if o.Kind == "remote" {
			stamp, haveStamp = myNegIdx.remoteFor(o.RoutingNumber, o.OfferID)
		} else {
			stamp, haveStamp = myNegIdx.localFor(o.LocalID)
		}
		if haveStamp {
			item.MyNegotiationId = stamp.id
			item.MyNegotiationStatus = stamp.status
		}
		// D2 — the listing is termless; re-source strike/premium/settlement per
		// viewer. BIDDER (not the owner, has a chain) → that chain's current
		// terms. OWNER of a LOCAL offer → their most recent counter. Else empty.
		h.projectViewerTerms(item, o, stamp, haveStamp, actorPrincipalType, actorPrincipalID)
		out = append(out, item)
	}
	var lastRefreshUnix int64
	if !snap.LastRefresh.IsZero() {
		lastRefreshUnix = snap.LastRefresh.Unix()
	}
	_ = ctx
	return &pb.ListUnifiedOptionOffersResponse{
		Offers:          out,
		TotalCount:      total,
		PeersTotal:      int32(snap.PeersTotal),
		PeersReached:    int32(snap.PeersReached),
		Partial:         snap.PeersTotal > 0 && snap.PeersReached < snap.PeersTotal,
		LastRefreshUnix: lastRefreshUnix,
	}, nil
}

// projectViewerTerms re-sources strike/premium/settlement onto one unified
// offer item per viewer (D2). The listing itself is termless, so the cache
// leaves those fields empty; this fills them from the caller's position:
//
//   - BIDDER (me_owner==false with an own chain on this offer): the chain's
//     CURRENT terms (carried in the stamp).
//   - OWNER (me_owner==true on a LOCAL offer): the acting principal's most
//     recent counter via the wired owner-latest-counter source.
//   - neither / no chain / no counter: left empty (the cache value, "").
//
// Remote offers are never me_owner, so a bidder on a remote shell still gets
// the bidder branch. best_bid/best_ask/active_chains_count are untouched.
func (h *OTCHandler) projectViewerTerms(
	item *pb.UnifiedOptionOffer,
	o otccache.OptionOffer,
	stamp myNegStamp,
	haveStamp bool,
	actorPrincipalType string,
	actorPrincipalID uint64,
) {
	switch {
	case !item.MeOwner && haveStamp:
		item.StrikePrice = stamp.terms.StrikePrice
		item.Premium = stamp.terms.Premium
		item.SettlementDate = stamp.terms.SettlementDate
	case item.MeOwner && o.Kind == "local" && h.ownerLatestCounter != nil:
		terms, terr := h.ownerLatestCounter(o.LocalID, actorPrincipalType, actorPrincipalID)
		if terr != nil {
			log.Printf("WARN ListUnifiedOptionOffers: owner-latest-counter for offer %d failed (leaving terms empty): %v", o.LocalID, terr)
			return
		}
		if terms != nil {
			item.StrikePrice = terms.StrikePrice
			item.Premium = terms.Premium
			item.SettlementDate = terms.SettlementDate
		}
	}
}
