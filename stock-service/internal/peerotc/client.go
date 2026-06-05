// Package peerotc performs authenticated outbound HTTP to a peer bank's
// /negotiations API (SI-TX OTC egress). It mirrors the egress logic that
// previously lived in api-gateway's peer_otc_initiate_handler.go, now
// relocated here so stock-service handlers can drive cross-bank OTC flows
// without touching the gateway.
package peerotc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/exbanka/contract/sitxauth"
	transactionpb "github.com/exbanka/contract/transactionpb"
)

// PeerResolver resolves a peer bank code to the credentials needed to call
// its SI-TX /negotiations API.
//
// The concrete production implementation is adminResolver (defined in this
// file), which wraps transactionpb.PeerBankAdminServiceClient.ResolvePeerByBankCode.
// Tests supply a stub.
type PeerResolver interface {
	Resolve(ctx context.Context, bankCode string) (baseURL, apiToken, hmacKey string, active, found bool, err error)
}

// Client performs authenticated outbound HTTP to a peer's /negotiations API.
type Client struct {
	resolver    PeerResolver
	httpClient  *http.Client
	ownBankCode string
}

// New creates a Client. A nil httpClient is replaced with a 30-second-timeout
// default so callers don't need to supply one.
func New(resolver PeerResolver, httpClient *http.Client, ownBankCode string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{resolver: resolver, httpClient: httpClient, ownBankCode: ownBankCode}
}

// CreateNegotiation POSTs the composed SI-TX OtcOffer to
// {peer.base_url}/negotiations and returns the peer-assigned
// (routingNumber, id) pair.
func (c *Client) CreateNegotiation(ctx context.Context, peerBankCode string, offer map[string]any) (int64, string, error) {
	baseURL, apiKey, hmacKey, active, found, err := c.resolver.Resolve(ctx, peerBankCode)
	if err != nil {
		return 0, "", fmt.Errorf("resolve peer: %w", err)
	}
	if !found {
		return 0, "", fmt.Errorf("peer bank %s not registered", peerBankCode)
	}
	if !active {
		return 0, "", fmt.Errorf("peer bank %s inactive", peerBankCode)
	}

	body, err := json.Marshal(offer)
	if err != nil {
		return 0, "", fmt.Errorf("marshal offer: %w", err)
	}

	url := strings.TrimRight(baseURL, "/") + "/negotiations"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	sitxauth.Sign(req, apiKey, hmacKey, c.ownBankCode, body)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("peer dispatch: %w", err)
	}
	defer resp.Body.Close()

	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("peer rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}

	var pr struct {
		RoutingNumber int64  `json:"routingNumber"`
		ID            string `json:"id"`
	}
	if err := json.Unmarshal(rb, &pr); err != nil {
		return 0, "", fmt.Errorf("decode peer response: %w", err)
	}
	return pr.RoutingNumber, pr.ID, nil
}

// Proxy forwards a single-negotiation operation to
// {peer.base_url}/negotiations/{rid}/{foreignID}{subpath}.
//
// method is an HTTP verb (GET, PUT, DELETE, …).
// subpath is appended verbatim after foreignID (e.g. "/accept", ""); pass ""
// for leaf operations.
// body is the request body (nil/empty for GET, DELETE).
//
// Returns the raw response body, the HTTP status code, and any transport
// error. Non-2xx status codes are NOT treated as errors — callers pass the
// body+status straight through to the originating client.
func (c *Client) Proxy(ctx context.Context, peerBankCode, rid, foreignID, method, subpath string, body []byte) ([]byte, int, error) {
	baseURL, apiKey, hmacKey, active, found, err := c.resolver.Resolve(ctx, peerBankCode)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("resolve peer: %w", err)
	}
	if !found || !active {
		return nil, http.StatusFailedDependency, fmt.Errorf("peer bank %s inactive or unknown", peerBankCode)
	}

	url := strings.TrimRight(baseURL, "/") + "/negotiations/" + rid + "/" + foreignID + subpath
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	sitxauth.Sign(req, apiKey, hmacKey, c.ownBankCode, body)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("peer dispatch: %w", err)
	}
	defer resp.Body.Close()

	rb, _ := io.ReadAll(resp.Body)
	return rb, resp.StatusCode, nil
}

// NewAdminResolver returns a PeerResolver backed by the production
// transactionpb.PeerBankAdminServiceClient. It is the adapter that wires
// stock-service's gRPC connection to the PeerResolver interface.
func NewAdminResolver(c transactionpb.PeerBankAdminServiceClient) PeerResolver {
	return adminResolver{c: c}
}

type adminResolver struct {
	c transactionpb.PeerBankAdminServiceClient
}

func (a adminResolver) Resolve(ctx context.Context, bankCode string) (string, string, string, bool, bool, error) {
	resp, err := a.c.ResolvePeerByBankCode(ctx, &transactionpb.ResolvePeerByBankCodeRequest{BankCode: bankCode})
	if err != nil {
		return "", "", "", false, false, err
	}
	if !resp.GetFound() {
		return "", "", "", false, false, nil
	}
	p := resp.GetPeerBank()
	return p.GetBaseUrl(), p.GetApiTokenPlaintext(), p.GetHmacOutboundKey(), p.GetActive(), true, nil
}
