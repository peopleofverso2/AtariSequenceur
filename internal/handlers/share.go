// Sharing endpoints — invitation links + accept flow.
//
// Routes:
//   POST   /api/songs/{id}/share        body {email, role?} -> share row
//   GET    /api/songs/{id}/shares       -> list of pending/accepted shares
//   DELETE /api/shares/{shareId}        -> revoke a share you own
//   POST   /api/shares/accept/{token}   -> accept an invitation as the
//                                          currently authenticated user
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/peopleofverso/atari-step-sequencer/internal/auth"
	"github.com/peopleofverso/atari-step-sequencer/internal/store"
)
// Mailer is satisfied by *mailer.Brevo (import elided to avoid a
// cycle and keep the package independently testable).
var _ Mailer = (interface {
	Configured() bool
	Send(context.Context, string, string, string) (string, error)
})(nil)

// AttachMailer wires a Brevo mailer into the API. We keep it optional —
// when the key is missing the share endpoint still works and returns
// the URL so the inviter can copy/paste it manually.
type Mailer interface {
	Configured() bool
	Send(ctx context.Context, to, subject, htmlBody string) (string, error)
}

// SetMailer is exported so main.go can attach the optional dependency
// after building the API (avoids forcing every test to inject one).
func (a *API) SetMailer(m Mailer, appURL string) {
	a.mailer = m
	a.appURL = strings.TrimRight(appURL, "/")
}

// shareInput is the payload of POST /api/songs/{id}/share.
type shareInput struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

func (in *shareInput) normalize() error {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if l := len(in.Email); l < 3 || l > 254 || !strings.Contains(in.Email, "@") {
		return errors.New("email is invalid")
	}
	if in.Role == "" {
		in.Role = "editor"
	}
	if in.Role != "editor" && in.Role != "viewer" {
		return errors.New("role must be 'editor' or 'viewer'")
	}
	return nil
}

func randomToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ShareSong creates the invitation and (best-effort) sends the email.
func (a *API) ShareSong(w http.ResponseWriter, r *http.Request) {
	if !a.ready(w) {
		return
	}
	userID := auth.UserID(r.Context())
	songID := chi.URLParam(r, "id")

	// Only the owner can share.
	role, err := a.store.SongAccess(r.Context(), userID, songID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not check song access")
		return
	}
	if role != "owner" {
		writeErr(w, http.StatusForbidden, "only the song owner can invite")
		return
	}

	var in shareInput
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if err := in.normalize(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	tok, err := randomToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not generate token")
		return
	}
	sh, err := a.store.CreateShare(r.Context(), "song", songID, userID, in.Email, in.Role, tok)
	if err != nil {
		if errors.Is(err, store.ErrShareExists) {
			writeErr(w, http.StatusConflict, "an invitation for that email already exists")
			return
		}
		slog.Warn("share create failed", "err", err, "song", songID, "email", in.Email)
		writeErr(w, http.StatusInternalServerError, "could not create invitation")
		return
	}

	// Look up song name for the email body. Best-effort — we already
	// know the user has access.
	songName := "your shared song"
	if s, err := a.store.GetSong(r.Context(), userID, songID); err == nil {
		songName = s.Name
	}

	acceptURL := a.buildAcceptURL(tok)
	emailSent := false
	if a.mailer != nil && a.mailer.Configured() {
		ownerEmail := ""
		// Best-effort: fetch owner email for "X invited you" phrasing.
		if u, err := a.store.GetUserByID(r.Context(), userID); err == nil {
			ownerEmail = u.Email
		}
		body := inviteEmailHTML(ownerEmail, songName, acceptURL)
		subj := "Invitation à jouer sur « " + songName + " »"
		if _, err := a.mailer.Send(r.Context(), in.Email, subj, body); err != nil {
			slog.Warn("brevo send failed", "err", err, "to", in.Email)
		} else {
			emailSent = true
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"share":     sh,
		"acceptUrl": acceptURL,
		"emailSent": emailSent,
	})
}

// ListSongShares returns invitations emitted on a song the user owns.
func (a *API) ListSongShares(w http.ResponseWriter, r *http.Request) {
	if !a.ready(w) {
		return
	}
	userID := auth.UserID(r.Context())
	songID := chi.URLParam(r, "id")

	role, err := a.store.SongAccess(r.Context(), userID, songID)
	if err != nil || role != "owner" {
		writeErr(w, http.StatusForbidden, "only the song owner can list invitations")
		return
	}
	shares, err := a.store.ListSharesForResource(r.Context(), userID, "song", songID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list invitations")
		return
	}
	// Decorate each entry with the accept URL only when still pending.
	type out struct {
		store.Share
		AcceptURL string `json:"acceptUrl,omitempty"`
	}
	enriched := make([]out, len(shares))
	for i, sh := range shares {
		o := out{Share: sh}
		if sh.AcceptedAt == nil && sh.Token != "" {
			o.AcceptURL = a.buildAcceptURL(sh.Token)
		}
		enriched[i] = o
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": enriched})
}

// RevokeShare deletes an invitation owned by the user.
func (a *API) RevokeShare(w http.ResponseWriter, r *http.Request) {
	if !a.ready(w) {
		return
	}
	userID := auth.UserID(r.Context())
	if err := a.store.DeleteShare(r.Context(), userID, chi.URLParam(r, "shareId")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "invitation not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "could not revoke")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AcceptShare binds the share to the logged-in user. Returns the
// share row + the resource id so the frontend can navigate to it.
func (a *API) AcceptShare(w http.ResponseWriter, r *http.Request) {
	if !a.ready(w) {
		return
	}
	userID := auth.UserID(r.Context())
	tok := chi.URLParam(r, "token")
	// Look up the user's email — we mirror it into the share row.
	u, err := a.store.GetUserByID(r.Context(), userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load account")
		return
	}
	sh, err := a.store.AcceptShare(r.Context(), tok, userID, u.Email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "invitation not found or expired")
			return
		}
		writeErr(w, http.StatusInternalServerError, "could not accept")
		return
	}
	writeJSON(w, http.StatusOK, sh)
}

// PreviewShare returns minimal info about an invitation token — used by
// the public accept page before the user logs in (so they know who
// invited them and what song).
func (a *API) PreviewShare(w http.ResponseWriter, r *http.Request) {
	if !a.ready(w) {
		return
	}
	tok := chi.URLParam(r, "token")
	sh, err := a.store.GetShareByToken(r.Context(), tok)
	if err != nil {
		writeErr(w, http.StatusNotFound, "invitation not found")
		return
	}
	songName := ""
	if s, err := a.store.GetSongUnchecked(r.Context(), sh.ResourceID); err == nil {
		songName = s.Name
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resourceType": sh.ResourceType,
		"resourceId":   sh.ResourceID,
		"resourceName": songName,
		"ownerEmail":   sh.OwnerEmail,
		"inviteeEmail": sh.InviteeEmail,
		"alreadyAccepted": sh.AcceptedAt != nil,
	})
}

func (a *API) buildAcceptURL(token string) string {
	base := a.appURL
	if base == "" {
		base = "" // relative URL — the frontend resolves against window.location
	}
	return base + "/accept/" + url.PathEscape(token)
}

func inviteEmailHTML(ownerEmail, songName, acceptURL string) string {
	// Minimal HTML — most clients render this cleanly.
	return `<div style="font-family:Helvetica,Arial,sans-serif;max-width:540px;line-height:1.5">
  <p>` + html.EscapeString(ownerEmail) + ` t'invite à jouer sur la song
     <b>` + html.EscapeString(songName) + `</b> dans STEP·SEQ.</p>
  <p><a href="` + html.EscapeString(acceptURL) + `"
        style="display:inline-block;padding:10px 14px;background:#1c1834;
               color:#ffd86b;text-decoration:none;border:1px solid #000">
        Rejoindre la session</a></p>
  <p style="font-size:11px;color:#666">Ou copie-colle ce lien :<br>
     <code>` + html.EscapeString(acceptURL) + `</code></p>
</div>`
}

// Helper used by main.go — wires the share routes onto an authenticated
// chi.Router. Kept here so main.go stays small.
func (a *API) MountShareRoutes(r chi.Router) {
	r.Post("/songs/{id}/share", a.ShareSong)
	r.Get("/songs/{id}/shares", a.ListSongShares)
	r.Delete("/shares/{shareId}", a.RevokeShare)
	r.Post("/shares/accept/{token}", a.AcceptShare)
}

// PreviewShareRoute is mounted OUTSIDE the auth middleware so the
// accept page can fetch invitation metadata before login.
func (a *API) MountSharePreviewRoute(r chi.Router) {
	r.Get("/shares/preview/{token}", a.PreviewShare)
}

// helper: fmt for "couldn't" messages — saves a few lines elsewhere.
var _ = fmt.Sprintf
