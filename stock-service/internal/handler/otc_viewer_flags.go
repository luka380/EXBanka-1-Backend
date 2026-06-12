// Package handler — viewer-relative OTC negotiation flags.
//
// These are computed PER CALLER (like me_owner / my_negotiation_id) so the FE can
// render Accept / Counter / Reject / Withdraw buttons and "latest / mine" markers
// without re-deriving the SI-TX turn rules. They never change the underlying data;
// they are a projection of an existing chain onto the asking identity.
package handler

import (
	"encoding/json"

	contractsitx "github.com/exbanka/contract/sitx"
	stockpb "github.com/exbanka/contract/stockpb"
	"github.com/exbanka/stock-service/internal/model"
)

// ownerFromPrincipal maps a revision/action author's PRINCIPAL (the wire/local
// representation) onto an OWNER pair so it can be compared with ownerMatches:
//
//	"client"   + id → (client, id)
//	"employee"      → (bank, nil)   — every employee acts AS the bank
//	anything else   → passthrough (covers the remote "buyer"/"seller" roles, which
//	                  the callers handle before reaching here)
func ownerFromPrincipal(principalType string, principalID uint64) (model.OwnerType, *uint64) {
	switch principalType {
	case "client":
		idCopy := principalID
		return model.OwnerClient, &idCopy
	case "employee":
		return model.OwnerBank, nil
	default:
		return model.OwnerType(principalType), nil
	}
}

// ownerEquals compares two (owner_type, owner_id) pairs (a nil id == the bank
// sentinel; both-nil ⇒ equal). Mirrors service.ownerMatches, which is unexported.
func ownerEquals(t1 model.OwnerType, id1 *uint64, t2 model.OwnerType, id2 *uint64) bool {
	if t1 != t2 {
		return false
	}
	if id1 == nil && id2 == nil {
		return true
	}
	if id1 == nil || id2 == nil {
		return false
	}
	return *id1 == *id2
}

// localLastActionMine reports whether the caller authored a LOCAL chain's last
// action (the persisted LastActionByOwner* is owner-level, so a direct compare).
func localLastActionMine(n *model.OTCNegotiation, ot model.OwnerType, oid *uint64) bool {
	return ownerEquals(model.OwnerType(n.LastActionByOwnerType), n.LastActionByOwnerID, ot, oid)
}

// remoteLastActionMine reports whether OUR (local) side authored a REMOTE chain's
// last action: the wire LastModifiedBy.routing equals our own routing. The caller
// is always the local party on a remote mirror, so "local side last-moved" ⇔
// "the caller last-moved".
func remoteLastActionMine(n *model.OTCNegotiation, ownRouting int64) bool {
	var offer contractsitx.OtcOffer
	if err := json.Unmarshal([]byte(remoteOfferJSONOf(n)), &offer); err != nil {
		return false
	}
	return offer.LastModifiedBy.RoutingNumber == ownRouting
}

// stampNegotiationViewerFlags fills the viewer-relative action hints on a
// negotiation response from the caller's role + whether the caller made the last
// move. A non-party viewer (viewerRole "") leaves every action flag false.
func stampNegotiationViewerFlags(item *stockpb.OTCNegotiationResponse, viewerRole string, lastActionMine bool) {
	if item == nil {
		return
	}
	item.ViewerRole = viewerRole
	if viewerRole != "bidder" && viewerRole != "poster" {
		return // read-only / non-party (e.g. an employee browsing a client's listing)
	}
	item.LastActionMine = lastActionMine
	live := item.GetStatus() == "open" || item.GetStatus() == "countered"
	item.AwaitingViewer = live && !lastActionMine
	// Accept / Counter are turn-based: available only when the OTHER side made the
	// last move (matches the server's "acceptor opposite to last proposer" rule and
	// the cross-bank counter turn-guard).
	item.CanAccept = item.AwaitingViewer
	item.CanCounter = item.AwaitingViewer
	// Decline actions are turn-independent: the poster may reject a bid on their
	// listing; the bidder may withdraw their own chain — any time it is still live.
	item.CanReject = live && viewerRole == "poster"
	item.CanWithdraw = live && viewerRole == "bidder"
}

// stampRevisionViewerFlags sets mine (via the supplied predicate over the SOURCE
// row, index-aligned with out) and is_latest (the highest revision_number) on a
// revision list. out[i] corresponds to src[i] (revsToProto preserves order).
func stampRevisionViewerFlags(out []*stockpb.OTCNegotiationRevisionResponse, src []model.OTCNegotiationRevision, mine func(model.OTCNegotiationRevision) bool) {
	maxRev := int32(-1)
	maxIdx := -1
	for i := range out {
		if i < len(src) {
			out[i].Mine = mine(src[i])
		}
		if out[i].GetRevisionNumber() > maxRev {
			maxRev = out[i].GetRevisionNumber()
			maxIdx = i
		}
	}
	if maxIdx >= 0 {
		out[maxIdx].IsLatest = true
	}
}

// stampTimelineViewerFlags sets mine + is_latest on a cross-chain timeline.
// mine: the timeline's caller authored the entry. For a LOCAL entry the author
// principal is matched by owner; for a REMOTE entry the role ("buyer"/"seller") is
// compared to the caller's own remote role — viewerRemoteRole is "seller" when the
// caller is the listing POSTER (the local-listing timeline) and "buyer" when the
// caller is the BIDDER (the remote-listing timeline, surfacing the caller's own
// chains). is_latest is per chain (the highest revision_number for each
// negotiation_id), so each chain's current terms are flagged in the merged view.
func stampTimelineViewerFlags(timeline []*stockpb.OTCTimelineEntry, ot model.OwnerType, oid *uint64, viewerRemoteRole string) {
	maxRevByNeg := map[uint64]int32{}
	maxIdxByNeg := map[uint64]int{}
	for i, e := range timeline {
		switch e.GetActionByPrincipalType() {
		case "buyer", "seller": // remote role
			e.Mine = e.GetActionByPrincipalType() == viewerRemoteRole
		default: // local principal ("client"/"employee")
			at, aid := ownerFromPrincipal(e.GetActionByPrincipalType(), e.GetActionByPrincipalId())
			e.Mine = ownerEquals(at, aid, ot, oid)
		}
		nid := e.GetNegotiationId()
		if cur, ok := maxRevByNeg[nid]; !ok || e.GetRevisionNumber() > cur {
			maxRevByNeg[nid] = e.GetRevisionNumber()
			maxIdxByNeg[nid] = i
		}
	}
	for _, idx := range maxIdxByNeg {
		timeline[idx].IsLatest = true
	}
}
