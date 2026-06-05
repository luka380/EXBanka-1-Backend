package sitxauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"
)

// Sign sets the SI-TX peer-auth headers on req. X-Api-Key is always set.
// When hmacOutboundKey != "", the HMAC bundle is added: X-Bank-Code (the
// SENDER's own bank code), X-Bank-Signature = HMAC-SHA256(hmacOutboundKey, body)
// hex-encoded, X-Timestamp (RFC3339 UTC now), X-Nonce (16 random bytes hex).
// body is the exact request body bytes (use nil/[]byte{} for GET).
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
