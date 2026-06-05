package peerotc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/exbanka/stock-service/internal/peerotc"
)

// stubResolver implements PeerResolver for tests.
type stubResolver struct {
	baseURL  string
	apiToken string
	hmacKey  string
	active   bool
	found    bool
	err      error
}

func (s stubResolver) Resolve(_ context.Context, _ string) (string, string, string, bool, bool, error) {
	return s.baseURL, s.apiToken, s.hmacKey, s.active, s.found, s.err
}

// okResolver returns a ready-to-use resolver pointing at the given server.
func okResolver(srv *httptest.Server) stubResolver {
	return stubResolver{
		baseURL:  srv.URL,
		apiToken: "test-api-key",
		hmacKey:  "test-hmac-key",
		active:   true,
		found:    true,
	}
}

// --- CreateNegotiation tests ---

func TestCreateNegotiation_Success(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	var gotApiKey, gotSig string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotApiKey = r.Header.Get("X-Api-Key")
		gotSig = r.Header.Get("X-Bank-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"routingNumber": int64(42),
			"id":            "negotiation-abc",
		})
	}))
	defer srv.Close()

	client := peerotc.New(okResolver(srv), nil, "111")

	offer := map[string]any{"listingId": "stock-1", "price": 100.0}
	routingNumber, id, err := client.CreateNegotiation(context.Background(), "222", offer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Returned values match server response.
	if routingNumber != 42 {
		t.Errorf("routingNumber: got %d, want 42", routingNumber)
	}
	if id != "negotiation-abc" {
		t.Errorf("id: got %q, want %q", id, "negotiation-abc")
	}

	// Server received POST /negotiations.
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %s, want POST", gotMethod)
	}
	if gotPath != "/negotiations" {
		t.Errorf("path: got %q, want /negotiations", gotPath)
	}

	// Auth headers present.
	if gotApiKey != "test-api-key" {
		t.Errorf("X-Api-Key: got %q, want test-api-key", gotApiKey)
	}
	if gotSig == "" {
		t.Error("X-Bank-Signature header missing")
	}

	// Body matches marshalled offer.
	var sentOffer map[string]any
	if err := json.Unmarshal(gotBody, &sentOffer); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if sentOffer["listingId"] != "stock-1" {
		t.Errorf("body listingId: got %v", sentOffer["listingId"])
	}
}

func TestCreateNegotiation_Non2xxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request from peer", http.StatusBadRequest)
	}))
	defer srv.Close()

	client := peerotc.New(okResolver(srv), nil, "111")
	_, _, err := client.CreateNegotiation(context.Background(), "222", map[string]any{"x": 1})
	if err == nil {
		t.Fatal("expected error for non-2xx status, got nil")
	}
}

func TestCreateNegotiation_PeerNotFound(t *testing.T) {
	resolver := stubResolver{found: false, active: false}
	client := peerotc.New(resolver, nil, "111")
	_, _, err := client.CreateNegotiation(context.Background(), "999", map[string]any{})
	if err == nil {
		t.Fatal("expected error when peer not found")
	}
}

func TestCreateNegotiation_PeerInactive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Should never be reached.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resolver := stubResolver{baseURL: srv.URL, apiToken: "k", active: false, found: true}
	client := peerotc.New(resolver, nil, "111")
	_, _, err := client.CreateNegotiation(context.Background(), "222", map[string]any{})
	if err == nil {
		t.Fatal("expected error when peer inactive")
	}
}

func TestCreateNegotiation_ResolverError(t *testing.T) {
	resolver := stubResolver{err: fmt.Errorf("grpc timeout")}
	client := peerotc.New(resolver, nil, "111")
	_, _, err := client.CreateNegotiation(context.Background(), "222", map[string]any{})
	if err == nil {
		t.Fatal("expected error when resolver fails")
	}
}

// --- Proxy tests ---

func proxyServer(t *testing.T) (*httptest.Server, *string, *string, *string) {
	t.Helper()
	var method, path string
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	return srv, &method, &path, &body
}

func TestProxy_PutNegotiation(t *testing.T) {
	srv, gotMethod, gotPath, gotBody := proxyServer(t)
	defer srv.Close()

	client := peerotc.New(okResolver(srv), nil, "111")
	payload := []byte(`{"price":99}`)
	rb, status, err := client.Proxy(context.Background(), "222", "42", "neg-id-1", http.MethodPut, "", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status: got %d, want 200", status)
	}
	if string(rb) != `{"status":"ok"}` {
		t.Errorf("body passthrough: got %q", string(rb))
	}
	if *gotMethod != http.MethodPut {
		t.Errorf("method: got %s, want PUT", *gotMethod)
	}
	if *gotPath != "/negotiations/42/neg-id-1" {
		t.Errorf("path: got %q, want /negotiations/42/neg-id-1", *gotPath)
	}
	if *gotBody != string(payload) {
		t.Errorf("body: got %q, want %q", *gotBody, string(payload))
	}
}

func TestProxy_GetAccept(t *testing.T) {
	srv, gotMethod, gotPath, _ := proxyServer(t)
	defer srv.Close()

	client := peerotc.New(okResolver(srv), nil, "111")
	_, status, err := client.Proxy(context.Background(), "222", "42", "neg-id-2", http.MethodGet, "/accept", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status: got %d, want 200", status)
	}
	if *gotMethod != http.MethodGet {
		t.Errorf("method: got %s, want GET", *gotMethod)
	}
	if *gotPath != "/negotiations/42/neg-id-2/accept" {
		t.Errorf("path: got %q, want /negotiations/42/neg-id-2/accept", *gotPath)
	}
}

func TestProxy_DeleteNegotiation(t *testing.T) {
	srv, gotMethod, gotPath, _ := proxyServer(t)
	defer srv.Close()

	client := peerotc.New(okResolver(srv), nil, "111")
	_, status, err := client.Proxy(context.Background(), "222", "42", "neg-id-3", http.MethodDelete, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status: got %d, want 200", status)
	}
	if *gotMethod != http.MethodDelete {
		t.Errorf("method: got %s, want DELETE", *gotMethod)
	}
	if *gotPath != "/negotiations/42/neg-id-3" {
		t.Errorf("path: got %q, want /negotiations/42/neg-id-3", *gotPath)
	}
}

func TestProxy_PeerNotFoundOrInactive(t *testing.T) {
	resolver := stubResolver{found: false, active: false}
	client := peerotc.New(resolver, nil, "111")
	_, status, err := client.Proxy(context.Background(), "999", "1", "x", http.MethodGet, "", nil)
	if err == nil {
		t.Fatal("expected error when peer not found/inactive")
	}
	if status != http.StatusFailedDependency {
		t.Errorf("status: got %d, want 424", status)
	}
}

func TestProxy_StatusPassthrough(t *testing.T) {
	// Non-2xx from peer is NOT an error — it is passed through.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "conflict", http.StatusConflict)
	}))
	defer srv.Close()

	client := peerotc.New(okResolver(srv), nil, "111")
	_, status, err := client.Proxy(context.Background(), "222", "1", "y", http.MethodPut, "", []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if status != http.StatusConflict {
		t.Errorf("status: got %d, want 409", status)
	}
}

// TestProxy_AuthHeadersSet verifies X-Api-Key and X-Bank-Signature are sent
// on Proxy calls (same signing path as CreateNegotiation).
func TestProxy_AuthHeadersSet(t *testing.T) {
	var gotApiKey, gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotApiKey = r.Header.Get("X-Api-Key")
		gotSig = r.Header.Get("X-Bank-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := peerotc.New(okResolver(srv), nil, "111")
	_, _, err := client.Proxy(context.Background(), "222", "1", "z", http.MethodGet, "/accept", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotApiKey != "test-api-key" {
		t.Errorf("X-Api-Key: got %q, want test-api-key", gotApiKey)
	}
	if gotSig == "" {
		t.Error("X-Bank-Signature header missing on Proxy call")
	}
}
