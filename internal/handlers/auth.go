// Auth-related HTTP handlers: signup and login. These endpoints are
// the only ones inside /api/ that are NOT behind the JWT middleware.
package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/peopleofverso/atari-step-sequencer/internal/auth"
	"github.com/peopleofverso/atari-step-sequencer/internal/store"
)

// credentials is the accepted body for both signup and login.
type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// normalize lower-cases the email and enforces simple length rules. We
// do not validate the email format strictly: any RFC-correct check is
// either too lax or too strict for real-world inboxes; the unique
// constraint in Postgres backstops bad rows.
func (c *credentials) normalize() error {
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	if l := len(c.Email); l < 3 || l > 254 || !strings.Contains(c.Email, "@") {
		return errors.New("email is invalid")
	}
	if l := len(c.Password); l < 8 || l > 128 {
		return errors.New("password must be 8-128 characters")
	}
	return nil
}

func (a *API) authReady(w http.ResponseWriter) bool {
	if a.store == nil {
		writeErr(w, http.StatusServiceUnavailable, "auth storage is not configured")
		return false
	}
	if a.auth == nil || !a.auth.Configured() {
		writeErr(w, http.StatusServiceUnavailable, "auth is not configured")
		return false
	}
	return true
}

func decodeCreds(w http.ResponseWriter, r *http.Request) (credentials, bool) {
	var c credentials
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return c, false
	}
	if err := c.normalize(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return c, false
	}
	return c, true
}

// authResponse is the shape returned by both signup and login.
type authResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expiresAt"`
	User      struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

func (a *API) mintResponse(u store.User) (authResponse, error) {
	tok, err := a.auth.Sign(u.ID)
	if err != nil {
		return authResponse{}, err
	}
	res := authResponse{Token: tok, ExpiresAt: a.auth.ExpiresAt().Unix()}
	res.User.ID = u.ID
	res.User.Email = u.Email
	return res, nil
}

// Signup creates a new account and returns a fresh JWT. It also
// claims any pending share invitations addressed to this email so the
// friend lands in their bank automatically.
func (a *API) Signup(w http.ResponseWriter, r *http.Request) {
	if !a.authReady(w) {
		return
	}
	c, ok := decodeCreds(w, r)
	if !ok {
		return
	}
	hash, err := auth.HashPassword(c.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not hash password")
		return
	}
	u, err := a.store.CreateUser(r.Context(), c.Email, hash)
	if err != nil {
		if errors.Is(err, store.ErrEmailTaken) {
			writeErr(w, http.StatusConflict, "email already registered")
			return
		}
		writeErr(w, http.StatusInternalServerError, "could not create account")
		return
	}
	// Claim pending invitations addressed to this email (best-effort).
	if n, err := a.store.AcceptPendingSharesForEmail(r.Context(), u.ID, u.Email); err == nil && n > 0 {
		// Logged at the call site is enough.
		_ = n
	}
	res, err := a.mintResponse(u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not issue token")
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// Login verifies credentials and returns a fresh JWT.
func (a *API) Login(w http.ResponseWriter, r *http.Request) {
	if !a.authReady(w) {
		return
	}
	c, ok := decodeCreds(w, r)
	if !ok {
		return
	}
	u, err := a.store.GetUserByEmail(r.Context(), c.Email)
	if err != nil {
		// Same 401 for "unknown user" and "wrong password" so we don't
		// leak which emails exist.
		writeErr(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if err := auth.CheckPassword(u.PasswordHash, c.Password); err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	// Idempotent : claim any invitations that arrived after the
	// account was created.
	_, _ = a.store.AcceptPendingSharesForEmail(r.Context(), u.ID, u.Email)
	res, err := a.mintResponse(u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not issue token")
		return
	}
	writeJSON(w, http.StatusOK, res)
}
