// Package auth verifies Supabase-issued JWTs.
//
// Supabase signs access tokens with HS256 using the project's legacy JWT
// secret. Verification needs only HMAC-SHA256, so this package depends on
// the standard library alone — the auth path stays small and auditable.
//
// If your Supabase project has migrated to asymmetric signing keys
// (ES256/RS256 via JWKS), replace parse() with a JWKS-based verifier.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type ctxKey int

const userIDKey ctxKey = iota

var errInvalidToken = errors.New("invalid token")

// Verifier validates tokens signed with a shared HS256 secret.
type Verifier struct {
	secret []byte
}

// NewVerifier builds a Verifier from the Supabase JWT secret.
func NewVerifier(secret string) *Verifier {
	return &Verifier{secret: []byte(secret)}
}

type claims struct {
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
}

// Middleware rejects requests lacking a valid bearer token and stores the
// authenticated user id in the request context for downstream handlers.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(v.secret) == 0 {
			deny(w, http.StatusServiceUnavailable, "authentication is not configured")
			return
		}
		token := bearerToken(r)
		if token == "" {
			deny(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		c, err := v.parse(token)
		if err != nil {
			deny(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, c.Sub)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// UserID returns the authenticated user id placed in the context by
// Middleware, or the empty string if the request was not authenticated.
func UserID(ctx context.Context) string {
	id, _ := ctx.Value(userIDKey).(string)
	return id
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// parse verifies an HS256 JWT's signature and expiry.
func (v *Verifier) parse(token string) (*claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errInvalidToken
	}

	// Reject anything that is not HS256 to prevent algorithm-confusion
	// attacks (e.g. a token forged with alg "none").
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errInvalidToken
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Alg != "HS256" {
		return nil, errInvalidToken
	}

	// Recompute the signature and compare in constant time.
	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	expected := mac.Sum(nil)

	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(expected, got) {
		return nil, errInvalidToken
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errInvalidToken
	}
	var c claims
	if err := json.Unmarshal(payloadJSON, &c); err != nil || c.Sub == "" {
		return nil, errInvalidToken
	}
	if c.Exp != 0 && time.Now().Unix() >= c.Exp {
		return nil, errInvalidToken
	}
	return &c, nil
}

func deny(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}
