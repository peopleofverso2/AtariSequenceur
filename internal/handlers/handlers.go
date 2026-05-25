// Package handlers implements the JSON HTTP API for sequencer patterns.
package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/peopleofverso/atari-step-sequencer/internal/auth"
	"github.com/peopleofverso/atari-step-sequencer/internal/store"
)

// maxBody caps request bodies: a 16-step, multi-track grid is a few
// hundred bytes, so 64 KiB leaves generous headroom while bounding memory.
const maxBody = 64 * 1024

// API holds the dependencies of the pattern handlers.
type API struct {
	store *store.Store
}

// New builds an API. The store may be nil when DATABASE_URL is unset, in
// which case every handler responds 503.
func New(s *store.Store) *API { return &API{store: s} }

// patternInput is the accepted request payload for create/update.
type patternInput struct {
	Name  string          `json:"name"`
	BPM   int             `json:"bpm"`
	Swing int             `json:"swing"`
	Steps json.RawMessage `json:"steps"`
}

// normalize trims and validates the payload in place.
func (in *patternInput) normalize() error {
	in.Name = strings.TrimSpace(in.Name)
	switch {
	case in.Name == "" || len([]rune(in.Name)) > 60:
		return errors.New("name must be 1-60 characters")
	case in.BPM < 40 || in.BPM > 300:
		return errors.New("bpm must be between 40 and 300")
	case in.Swing < 0 || in.Swing > 75:
		return errors.New("swing must be between 0 and 75")
	case len(in.Steps) == 0 || !json.Valid(in.Steps):
		return errors.New("steps must be a valid json object")
	}
	return nil
}

// ListPatterns returns every pattern owned by the caller.
func (a *API) ListPatterns(w http.ResponseWriter, r *http.Request) {
	if !a.ready(w) {
		return
	}
	items, err := a.store.List(r.Context(), auth.UserID(r.Context()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load patterns")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"patterns": items})
}

// GetPattern returns one pattern by id.
func (a *API) GetPattern(w http.ResponseWriter, r *http.Request) {
	if !a.ready(w) {
		return
	}
	p, err := a.store.Get(r.Context(), auth.UserID(r.Context()), chi.URLParam(r, "id"))
	if a.handleErr(w, err, "could not load pattern") {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// CreatePattern stores a new pattern.
func (a *API) CreatePattern(w http.ResponseWriter, r *http.Request) {
	if !a.ready(w) {
		return
	}
	in, ok := decode(w, r)
	if !ok {
		return
	}
	p, err := a.store.Create(r.Context(), auth.UserID(r.Context()), in.toPattern())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save pattern")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// UpdatePattern overwrites an existing pattern.
func (a *API) UpdatePattern(w http.ResponseWriter, r *http.Request) {
	if !a.ready(w) {
		return
	}
	in, ok := decode(w, r)
	if !ok {
		return
	}
	p, err := a.store.Update(r.Context(), auth.UserID(r.Context()),
		chi.URLParam(r, "id"), in.toPattern())
	if a.handleErr(w, err, "could not update pattern") {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// DeletePattern removes a pattern.
func (a *API) DeletePattern(w http.ResponseWriter, r *http.Request) {
	if !a.ready(w) {
		return
	}
	err := a.store.Delete(r.Context(), auth.UserID(r.Context()), chi.URLParam(r, "id"))
	if a.handleErr(w, err, "could not delete pattern") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (in patternInput) toPattern() store.Pattern {
	return store.Pattern{Name: in.Name, BPM: in.BPM, Swing: in.Swing, Steps: in.Steps}
}

// ready guards handlers when storage is not configured.
func (a *API) ready(w http.ResponseWriter) bool {
	if a.store == nil {
		writeErr(w, http.StatusServiceUnavailable, "pattern storage is not configured")
		return false
	}
	return true
}

// handleErr maps store errors to HTTP responses; it returns true if it
// wrote a response and the caller should stop.
func (a *API) handleErr(w http.ResponseWriter, err error, genericMsg string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "pattern not found")
	default:
		writeErr(w, http.StatusInternalServerError, genericMsg)
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request) (patternInput, bool) {
	var in patternInput
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return in, false
	}
	if err := in.normalize(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return in, false
	}
	return in, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
