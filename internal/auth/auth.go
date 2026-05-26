// Package auth handles password hashing, JWT signing and JWT verification.
//
// Tokens are HS256 with a per-deployment secret (env JWT_SECRET). Both
// signing and verification rely on the standard library + bcrypt — the
// auth path stays small and auditable.
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

	"golang.org/x/crypto/bcrypt"
)

type ctxKey int

const userIDKey ctxKey = iota

// DefaultTTL is the lifetime applied to newly minted tokens when no TTL
// is supplied. 30 days mirrors a "remember me" web session.
const DefaultTTL = 30 * 24 * time.Hour

// ErrBadCredentials is returned when an email/password pair fails. The
// caller maps it to a 401 without leaking which half was wrong.
var ErrBadCredentials = errors.New("invalid email or password")

var errInvalidToken = errors.New("invalid token")

// Auth signs and verifies HS256 JWTs and hashes passwords with bcrypt.
type Auth struct {
	secret []byte
	ttl    time.Duration
}

// New builds an Auth backed by the given shared secret.
func New(secret string) *Auth {
	return &Auth{secret: []byte(secret), ttl: DefaultTTL}
}

// Configured reports whether a non-empty secret was supplied. Handlers
// use it to fail fast with a 503 when auth is unavailable.
func (a *Auth) Configured() bool { return len(a.secret) > 0 }

// HashPassword returns a bcrypt hash suitable for storage.
func HashPassword(plaintext string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
}

// CheckPassword reports whether a bcrypt hash matches the candidate.
// It maps every failure to ErrBadCredentials so handlers don't have to
// distinguish between a missing user and a wrong password.
func CheckPassword(hash []byte, candidate string) error {
	if err := bcrypt.CompareHashAndPassword(hash, []byte(candidate)); err != nil {
		return ErrBadCredentials
	}
	return nil
}

// Sign returns a fresh HS256 token for the given user id.
func (a *Auth) Sign(userID string) (string, error) {
	if !a.Configured() {
		return "", errors.New("auth: secret not configured")
	}
	header := `{"alg":"HS256","typ":"JWT"}`
	exp := time.Now().Add(a.ttl).Unix()
	payload, err := json.Marshal(claims{Sub: userID, Exp: exp})
	if err != nil {
		return "", err
	}
	h64 := base64.RawURLEncoding.EncodeToString([]byte(header))
	p64 := base64.RawURLEncoding.EncodeToString(payload)
	signing := h64 + "." + p64

	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(signing))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signing + "." + sig, nil
}

// ExpiresAt returns the absolute expiry of tokens minted right now.
// Handy for the login response so the client can plan a refresh.
func (a *Auth) ExpiresAt() time.Time { return time.Now().Add(a.ttl) }

type claims struct {
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
}

// Middleware rejects requests lacking a valid bearer token and stores
// the authenticated user id in the request context.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Configured() {
			deny(w, http.StatusServiceUnavailable, "authentication is not configured")
			return
		}
		token := bearerToken(r)
		if token == "" {
			deny(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		c, err := a.parse(token)
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
func (a *Auth) parse(token string) (*claims, error) {
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
	mac := hmac.New(sha256.New, a.secret)
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
