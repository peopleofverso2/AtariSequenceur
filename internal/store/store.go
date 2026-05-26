// Package store persists sequencer patterns in Supabase Postgres.
//
// Every query is scoped by user_id. Row Level Security in the database is a
// second line of defence; this package enforces ownership in the query
// itself so a backend bug cannot leak another user's patterns.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a pattern does not exist or is not owned by
// the requesting user.
var ErrNotFound = errors.New("pattern not found")

// Pattern is a saved sequencer grid. Steps holds the raw JSON object
// mapping each track id to its array of on/off steps.
type Pattern struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	BPM       int             `json:"bpm"`
	Swing     int             `json:"swing"`
	Steps     json.RawMessage `json:"steps"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// Store provides pattern persistence over a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps a pgx pool in a Store.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const columns = `id, name, bpm, swing, steps, updated_at`

func scan(row pgx.Row) (Pattern, error) {
	var p Pattern
	err := row.Scan(&p.ID, &p.Name, &p.BPM, &p.Swing, &p.Steps, &p.UpdatedAt)
	return p, err
}

// List returns every pattern owned by the user, newest first.
func (s *Store) List(ctx context.Context, userID string) ([]Pattern, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+`
		   FROM patterns
		  WHERE user_id = $1
		  ORDER BY updated_at DESC
		  LIMIT 200`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Pattern, 0, 16)
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns a single pattern owned by the user.
func (s *Store) Get(ctx context.Context, userID, id string) (Pattern, error) {
	p, err := scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+`
		   FROM patterns
		  WHERE user_id = $1 AND id = $2`, userID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Pattern{}, ErrNotFound
	}
	return p, err
}

// Create inserts a new pattern and returns it with its generated id.
func (s *Store) Create(ctx context.Context, userID string, p Pattern) (Pattern, error) {
	return scan(s.pool.QueryRow(ctx,
		`INSERT INTO patterns (user_id, name, bpm, swing, steps)
		      VALUES ($1, $2, $3, $4, $5)
		   RETURNING `+columns,
		userID, p.Name, p.BPM, p.Swing, p.Steps))
}

// Update overwrites an existing pattern owned by the user.
func (s *Store) Update(ctx context.Context, userID, id string, p Pattern) (Pattern, error) {
	res, err := scan(s.pool.QueryRow(ctx,
		`UPDATE patterns
		    SET name = $3, bpm = $4, swing = $5, steps = $6, updated_at = now()
		  WHERE user_id = $1 AND id = $2
		 RETURNING `+columns,
		userID, id, p.Name, p.BPM, p.Swing, p.Steps))
	if errors.Is(err, pgx.ErrNoRows) {
		return Pattern{}, ErrNotFound
	}
	return res, err
}

// Delete removes a pattern owned by the user.
func (s *Store) Delete(ctx context.Context, userID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM patterns WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Song is an ordered playlist of patterns. Items is the raw JSON array
// [{ patternId, repeats }, ...]; patternId may reference a cloud uuid or
// a local-only id, so resolution is done client-side.
type Song struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Items     json.RawMessage `json:"items"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

const songCols = `id, name, items, updated_at`

func scanSong(row pgx.Row) (Song, error) {
	var s Song
	err := row.Scan(&s.ID, &s.Name, &s.Items, &s.UpdatedAt)
	return s, err
}

// ListSongs returns every song owned by the user, newest first.
func (s *Store) ListSongs(ctx context.Context, userID string) ([]Song, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+songCols+`
		   FROM songs
		  WHERE user_id = $1
		  ORDER BY updated_at DESC
		  LIMIT 200`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Song, 0, 16)
	for rows.Next() {
		song, err := scanSong(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, song)
	}
	return out, rows.Err()
}

// GetSong returns a single song owned by the user.
func (s *Store) GetSong(ctx context.Context, userID, id string) (Song, error) {
	song, err := scanSong(s.pool.QueryRow(ctx,
		`SELECT `+songCols+`
		   FROM songs
		  WHERE user_id = $1 AND id = $2`, userID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Song{}, ErrNotFound
	}
	return song, err
}

// CreateSong inserts a new song and returns it with its generated id.
func (s *Store) CreateSong(ctx context.Context, userID string, in Song) (Song, error) {
	return scanSong(s.pool.QueryRow(ctx,
		`INSERT INTO songs (user_id, name, items)
		      VALUES ($1, $2, $3)
		   RETURNING `+songCols,
		userID, in.Name, in.Items))
}

// UpdateSong overwrites an existing song owned by the user.
func (s *Store) UpdateSong(ctx context.Context, userID, id string, in Song) (Song, error) {
	res, err := scanSong(s.pool.QueryRow(ctx,
		`UPDATE songs
		    SET name = $3, items = $4, updated_at = now()
		  WHERE user_id = $1 AND id = $2
		 RETURNING `+songCols,
		userID, id, in.Name, in.Items))
	if errors.Is(err, pgx.ErrNoRows) {
		return Song{}, ErrNotFound
	}
	return res, err
}

// DeleteSong removes a song owned by the user.
func (s *Store) DeleteSong(ctx context.Context, userID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM songs WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
