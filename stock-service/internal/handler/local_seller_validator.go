package handler

import (
	"context"
	"strconv"
	"strings"

	clientpb "github.com/exbanka/contract/clientpb"
	"google.golang.org/grpc"
)

// ClientServiceLookup is the subset of clientpb.ClientServiceClient the seller
// validator uses. Declared as an interface so tests can stub it without a gRPC
// connection; the real clientpb.ClientServiceClient satisfies it.
type ClientServiceLookup interface {
	GetClient(ctx context.Context, in *clientpb.GetClientRequest, opts ...grpc.CallOption) (*clientpb.ClientResponse, error)
}

// ClientSellerValidator is the production LocalSellerValidator: it confirms a
// client-<n> participant id resolves to a real client on this bank via
// client-service GetClient. It is consulted only for the client-<n> form
// (bank/employee-<n> are validated structurally by the handler).
type ClientSellerValidator struct {
	clients ClientServiceLookup
}

// NewClientSellerValidator wires a ClientSellerValidator against a
// client-service client.
func NewClientSellerValidator(clients ClientServiceLookup) *ClientSellerValidator {
	return &ClientSellerValidator{clients: clients}
}

// SellerExists reports whether participantID (a "client-<n>" id) resolves to a
// real client on this bank. A parse failure or a client-service error/empty
// response is treated as non-existent (fail closed). A non-client id returns
// true — the handler only ever passes client-<n> here, but being lenient on the
// other forms keeps the contract simple.
func (v *ClientSellerValidator) SellerExists(ctx context.Context, participantID string) bool {
	rest, ok := strings.CutPrefix(participantID, "client-")
	if !ok {
		return true // not a client id — handler validates bank/employee structurally
	}
	id, err := strconv.ParseUint(rest, 10, 64)
	if err != nil || id == 0 {
		return false
	}
	resp, gerr := v.clients.GetClient(ctx, &clientpb.GetClientRequest{Id: id})
	if gerr != nil || resp == nil {
		return false
	}
	return resp.GetId() == id
}
