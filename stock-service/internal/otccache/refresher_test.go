package otccache

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"google.golang.org/grpc"

	contractsitx "github.com/exbanka/contract/sitx"
	transactionpb "github.com/exbanka/contract/transactionpb"
	"github.com/exbanka/stock-service/internal/model"
	"github.com/exbanka/stock-service/internal/service"
)

// fakeOTCLister returns canned holdings for fetchLocal.
type fakeOTCLister struct {
	rows []model.Holding
	err  error
}

func (f *fakeOTCLister) ListOffers(_ service.OTCFilter) ([]model.Holding, int64, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.rows, int64(len(f.rows)), nil
}

// fakePeerBankAdminClient implements transactionpb.PeerBankAdminServiceClient
// for the cache refresh path. As of the 2026-06-07 interbank cutover the
// refresher only calls ListPeerBanks (to enumerate which peers to poll); the
// per-peer HTTP fetch goes through PeerEgressService, so resolution methods are
// no longer exercised here and are stubbed not-used.
type fakePeerBankAdminClient struct {
	listResp *transactionpb.ListPeerBanksResponse
	listErr  error
}

func (f *fakePeerBankAdminClient) ListPeerBanks(ctx context.Context, in *transactionpb.ListPeerBanksRequest, opts ...grpc.CallOption) (*transactionpb.ListPeerBanksResponse, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResp, nil
}
func (f *fakePeerBankAdminClient) GetPeerBank(ctx context.Context, in *transactionpb.GetPeerBankRequest, opts ...grpc.CallOption) (*transactionpb.PeerBank, error) {
	return nil, errors.New("not used")
}
func (f *fakePeerBankAdminClient) CreatePeerBank(ctx context.Context, in *transactionpb.CreatePeerBankRequest, opts ...grpc.CallOption) (*transactionpb.PeerBank, error) {
	return nil, errors.New("not used")
}
func (f *fakePeerBankAdminClient) UpdatePeerBank(ctx context.Context, in *transactionpb.UpdatePeerBankRequest, opts ...grpc.CallOption) (*transactionpb.PeerBank, error) {
	return nil, errors.New("not used")
}
func (f *fakePeerBankAdminClient) DeletePeerBank(ctx context.Context, in *transactionpb.DeletePeerBankRequest, opts ...grpc.CallOption) (*transactionpb.DeletePeerBankResponse, error) {
	return nil, errors.New("not used")
}
func (f *fakePeerBankAdminClient) ResolvePeerByAPIToken(ctx context.Context, in *transactionpb.ResolvePeerByAPITokenRequest, opts ...grpc.CallOption) (*transactionpb.ResolvePeerByAPITokenResponse, error) {
	return nil, errors.New("not used")
}
func (f *fakePeerBankAdminClient) ResolvePeerByBankCode(ctx context.Context, in *transactionpb.ResolvePeerByBankCodeRequest, opts ...grpc.CallOption) (*transactionpb.ResolvePeerByBankCodeResponse, error) {
	return nil, errors.New("not used")
}

// fakePeerEgressClient implements transactionpb.PeerEgressServiceClient. The
// refresher's fetchPeer drives ProxyToPeer (GET /public-stock or
// /public-option-offers); CheckPeerReachability/GetPeersState are unused here.
type fakePeerEgressClient struct {
	resp    *transactionpb.ProxyToPeerResponse
	err     error
	gotReqs []*transactionpb.ProxyToPeerRequest
}

func (f *fakePeerEgressClient) ProxyToPeer(ctx context.Context, in *transactionpb.ProxyToPeerRequest, opts ...grpc.CallOption) (*transactionpb.ProxyToPeerResponse, error) {
	f.gotReqs = append(f.gotReqs, in)
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}
func (f *fakePeerEgressClient) CheckPeerReachability(ctx context.Context, in *transactionpb.CheckPeerReachabilityRequest, opts ...grpc.CallOption) (*transactionpb.PeerReachability, error) {
	return nil, errors.New("not used")
}
func (f *fakePeerEgressClient) GetPeersState(ctx context.Context, in *transactionpb.GetPeersStateRequest, opts ...grpc.CallOption) (*transactionpb.GetPeersStateResponse, error) {
	return nil, errors.New("not used")
}

// publicStocksBody marshals a PublicStocksResponse into the verbatim body an
// interbank ProxyToPeer call would return from a peer's GET /public-stock.
func publicStocksBody(t *testing.T, resp contractsitx.PublicStocksResponse) []byte {
	t.Helper()
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal public-stocks: %v", err)
	}
	return b
}

// TestNewRefresher builds a Refresher and verifies field defaults.
func TestNewRefresher(t *testing.T) {
	c := New()
	egress := &fakePeerEgressClient{}
	r := NewRefresher(c, &fakeOTCLister{}, &fakePeerBankAdminClient{}, egress, "111", 5*time.Minute)
	if r.cache != c {
		t.Errorf("cache wired wrong")
	}
	if r.ownBankCode != "111" {
		t.Errorf("ownBankCode = %q", r.ownBankCode)
	}
	if r.interval != 5*time.Minute {
		t.Errorf("interval = %v", r.interval)
	}
	if r.egress == nil {
		t.Error("egress client should be wired")
	}
}

// TestRefresher_FetchLocal_Maps verifies fetchLocal converts model.Holding
// rows to the unified Offer shape with the local bank code.
func TestRefresher_FetchLocal_Maps(t *testing.T) {
	uid := uint64(7)
	holding := model.Holding{
		ID: 99, OwnerID: &uid,
		UserFirstName: "Jane", UserLastName: "Doe",
		SecurityType:   "stock",
		Ticker:         "AAPL",
		Name:           "Apple",
		PublicQuantity: 5,
		AveragePrice:   decimal.NewFromFloat(123.45),
		CreatedAt:      time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
	}
	r := NewRefresher(New(), &fakeOTCLister{rows: []model.Holding{holding}}, nil, nil, "111", time.Minute)
	out, err := r.fetchLocal()
	if err != nil {
		t.Fatalf("fetchLocal: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d offers", len(out))
	}
	o := out[0]
	if o.Kind != "local" || o.BankCode != "111" {
		t.Errorf("kind=%s bank=%s", o.Kind, o.BankCode)
	}
	if o.SellerID != 7 || o.SellerName != "Jane Doe" {
		t.Errorf("seller=%d name=%q", o.SellerID, o.SellerName)
	}
	if o.PricePerUnit != "123.45" {
		t.Errorf("price=%q", o.PricePerUnit)
	}
	if o.Ticker != "AAPL" {
		t.Errorf("ticker=%q", o.Ticker)
	}
}

// TestRefresher_FetchLocal_ErrorPropagates ensures an error from the OTCLister
// surface propagates so the caller can log + skip the local source.
func TestRefresher_FetchLocal_ErrorPropagates(t *testing.T) {
	r := NewRefresher(New(), &fakeOTCLister{err: errors.New("boom")}, nil, nil, "111", time.Minute)
	if _, err := r.fetchLocal(); err == nil {
		t.Fatal("expected error")
	}
}

// TestRefresher_Refresh_LocalOnly_NoPeers covers the path where local fetch
// works and there are no peers — the cache should still be populated.
func TestRefresher_Refresh_LocalOnly_NoPeers(t *testing.T) {
	uid := uint64(7)
	holding := model.Holding{
		ID: 99, OwnerID: &uid, UserFirstName: "A", UserLastName: "B",
		SecurityType: "stock", Ticker: "AAPL",
		PublicQuantity: 5, AveragePrice: decimal.NewFromInt(100),
	}
	cache := New()
	peerAdmin := &fakePeerBankAdminClient{
		listResp: &transactionpb.ListPeerBanksResponse{PeerBanks: nil},
	}
	r := NewRefresher(cache, &fakeOTCLister{rows: []model.Holding{holding}}, peerAdmin, &fakePeerEgressClient{}, "111", time.Minute)
	r.refresh(context.Background())

	snap := cache.Get()
	if len(snap.Offers) != 1 {
		t.Fatalf("expected 1 offer, got %d", len(snap.Offers))
	}
	if snap.Offers[0].Kind != "local" {
		t.Errorf("expected local offer")
	}
	if snap.PeersTotal != 0 || snap.PeersReached != 0 {
		t.Errorf("peers total/reached = %d/%d", snap.PeersTotal, snap.PeersReached)
	}
	if snap.LastRefresh.IsZero() {
		t.Errorf("LastRefresh not set")
	}
}

// TestRefresher_Refresh_LocalFetchFails covers the log-and-continue branch in
// refresh when fetchLocal returns an error.
func TestRefresher_Refresh_LocalFetchFails(t *testing.T) {
	cache := New()
	peerAdmin := &fakePeerBankAdminClient{listResp: &transactionpb.ListPeerBanksResponse{PeerBanks: nil}}
	r := NewRefresher(cache, &fakeOTCLister{err: errors.New("local boom")}, peerAdmin, &fakePeerEgressClient{}, "111", time.Minute)
	r.refresh(context.Background())
	snap := cache.Get()
	if len(snap.Offers) != 0 {
		t.Errorf("expected 0 offers, got %d", len(snap.Offers))
	}
}

// TestRefresher_Refresh_PeerListFails covers the branch where ListPeerBanks
// returns an error — local offers still get cached.
func TestRefresher_Refresh_PeerListFails(t *testing.T) {
	cache := New()
	peerAdmin := &fakePeerBankAdminClient{listErr: errors.New("peer list fail")}
	r := NewRefresher(cache, &fakeOTCLister{}, peerAdmin, &fakePeerEgressClient{}, "111", time.Minute)
	r.refresh(context.Background())
	snap := cache.Get()
	if snap.PeersTotal != 0 {
		t.Errorf("peers total = %d (want 0 when list fails)", snap.PeersTotal)
	}
}

// TestRefresher_FetchPeer_HappyPath drives a fake interbank ProxyToPeer that
// returns 200 + PublicStocksResponse JSON, and asserts the offers come back as
// remote entries with the right bank code + ticker mapping. It also verifies
// the leaf request (GET /public-stock) handed to interbank.
func TestRefresher_FetchPeer_HappyPath(t *testing.T) {
	egress := &fakePeerEgressClient{
		resp: &transactionpb.ProxyToPeerResponse{
			StatusCode: http.StatusOK,
			Body: publicStocksBody(t, contractsitx.PublicStocksResponse{
				{
					Stock: contractsitx.StockDescription{Ticker: "MSFT"},
					Sellers: []contractsitx.PublicSeller{
						{Seller: contractsitx.ForeignBankId{RoutingNumber: 222, ID: "client-3"}, Amount: 50},
					},
				},
			}),
		},
	}
	r := NewRefresher(New(), &fakeOTCLister{}, &fakePeerBankAdminClient{}, egress, "111", time.Minute)
	out, err := r.fetchPeer(context.Background(), &transactionpb.PeerBank{BankCode: "222"})
	if err != nil {
		t.Fatalf("fetchPeer: %v", err)
	}
	if len(egress.gotReqs) != 1 || egress.gotReqs[0].GetPeerBankCode() != "222" ||
		egress.gotReqs[0].GetMethod() != http.MethodGet || egress.gotReqs[0].GetPath() != "/public-stock" {
		t.Fatalf("ProxyToPeer req = %+v", egress.gotReqs)
	}
	if len(out) != 1 {
		t.Fatalf("got %d offers", len(out))
	}
	if out[0].Kind != "remote" || out[0].BankCode != "222" || out[0].Ticker != "MSFT" {
		t.Errorf("offer = %+v", out[0])
	}
	if out[0].OwnerID != "client-3" {
		t.Errorf("owner_id = %q want client-3", out[0].OwnerID)
	}
}

// TestRefresher_FetchPeer_EgressError surfaces a gRPC error from interbank's
// ProxyToPeer. Post-cutover, an unknown/inactive/unreachable peer is signalled
// by interbank as an error (NotFound/FailedPrecondition/Unavailable), which the
// refresher logs and skips per-peer.
func TestRefresher_FetchPeer_EgressError(t *testing.T) {
	egress := &fakePeerEgressClient{err: errors.New("peer bank inactive")}
	r := NewRefresher(New(), &fakeOTCLister{}, &fakePeerBankAdminClient{}, egress, "111", time.Minute)
	_, err := r.fetchPeer(context.Background(), &transactionpb.PeerBank{BankCode: "222"})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestRefresher_FetchPeer_BadStatusCode surfaces a non-200 the peer returned
// (passed through verbatim by interbank as ProxyToPeerResponse.StatusCode).
func TestRefresher_FetchPeer_BadStatusCode(t *testing.T) {
	egress := &fakePeerEgressClient{resp: &transactionpb.ProxyToPeerResponse{
		StatusCode: http.StatusInternalServerError, Body: []byte("server error"),
	}}
	r := NewRefresher(New(), &fakeOTCLister{}, &fakePeerBankAdminClient{}, egress, "111", time.Minute)
	_, err := r.fetchPeer(context.Background(), &transactionpb.PeerBank{BankCode: "222"})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestRefresher_FetchPeer_BadJSON surfaces a JSON-parse error from the peer body.
func TestRefresher_FetchPeer_BadJSON(t *testing.T) {
	egress := &fakePeerEgressClient{resp: &transactionpb.ProxyToPeerResponse{
		StatusCode: http.StatusOK, Body: []byte("not json"),
	}}
	r := NewRefresher(New(), &fakeOTCLister{}, &fakePeerBankAdminClient{}, egress, "111", time.Minute)
	_, err := r.fetchPeer(context.Background(), &transactionpb.PeerBank{BankCode: "222"})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestRefresher_Refresh_WithReachablePeer wires a single peer (via ListPeerBanks)
// and a fake interbank egress, and verifies both local and remote offers land
// in cache with the reachability counters set.
func TestRefresher_Refresh_WithReachablePeer(t *testing.T) {
	egress := &fakePeerEgressClient{resp: &transactionpb.ProxyToPeerResponse{
		StatusCode: http.StatusOK,
		Body: publicStocksBody(t, contractsitx.PublicStocksResponse{
			{
				Stock: contractsitx.StockDescription{Ticker: "GOOG"},
				Sellers: []contractsitx.PublicSeller{
					{Seller: contractsitx.ForeignBankId{RoutingNumber: 222, ID: "0"}, Amount: 12},
				},
			},
		}),
	}}
	peerAdmin := &fakePeerBankAdminClient{
		listResp: &transactionpb.ListPeerBanksResponse{
			PeerBanks: []*transactionpb.PeerBank{{BankCode: "222"}},
		},
	}
	cache := New()
	uid := uint64(7)
	holding := model.Holding{
		OwnerID: &uid, UserFirstName: "X", UserLastName: "Y",
		SecurityType: "stock", Ticker: "AAPL",
		PublicQuantity: 1, AveragePrice: decimal.NewFromInt(150),
	}
	r := NewRefresher(cache, &fakeOTCLister{rows: []model.Holding{holding}}, peerAdmin, egress, "111", time.Minute)
	r.refresh(context.Background())
	snap := cache.Get()
	if len(snap.Offers) != 2 {
		t.Fatalf("expected 2 offers (1 local, 1 remote), got %d", len(snap.Offers))
	}
	if snap.PeersTotal != 1 || snap.PeersReached != 1 {
		t.Errorf("peers total/reached = %d/%d", snap.PeersTotal, snap.PeersReached)
	}
}

// TestSetForTest verifies the test seam writes through to internal state.
func TestSetForTest(t *testing.T) {
	c := New()
	SetForTest(c, Snapshot{
		Offers:       []Offer{{Kind: "remote", Ticker: "X"}},
		LastRefresh:  time.Now(),
		PeersTotal:   2,
		PeersReached: 1,
	})
	got := c.Get()
	if len(got.Offers) != 1 || got.Offers[0].Ticker != "X" {
		t.Errorf("got %+v", got.Offers)
	}
	if got.PeersTotal != 2 || got.PeersReached != 1 {
		t.Errorf("peers totals wrong")
	}
}

// TestRefresher_Run_StopsOnContextCancel verifies the goroutine returns when
// context is cancelled — the test must not hang.
func TestRefresher_Run_StopsOnContextCancel(t *testing.T) {
	cache := New()
	peerAdmin := &fakePeerBankAdminClient{listResp: &transactionpb.ListPeerBanksResponse{PeerBanks: nil}}
	r := NewRefresher(cache, &fakeOTCLister{}, peerAdmin, &fakePeerEgressClient{}, "111", 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
