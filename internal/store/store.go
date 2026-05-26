// Package store persists users, patterns and songs in Postgres.
//
// Pattern and song queries are scoped by user_id at the SQL layer so a
// handler bug cannot leak another account's data. User rows live in
// their own table and are referenced by the patterns/songs FK.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a row does not exist (or, for owned rows,
// is not owned by the requesting user).
var ErrNotFound = errors.New("not found")

// ErrEmailTaken is returned by CreateUser when the email is already used.
var ErrEmailTaken = errors.New("email already registered")

// User is an application account. PasswordHash holds the bcrypt digest.
type User struct {
	ID           string
	Email        string
	PasswordHash []byte
	CreatedAt    time.Time
}

// CreateUser inserts a new user. The unique constraint on email is mapped
// to ErrEmailTaken so handlers can return a clear 409.
func (s *Store) CreateUser(ctx context.Context, email string, passwordHash []byte) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash)
		      VALUES ($1, $2)
		   RETURNING id, email, password_hash, created_at`,
		email, passwordHash).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		// pgx surfaces "23505" (unique_violation) via PgError.Code, but
		// importing the pgconn package just to type-assert is overkill;
		// a substring check on the error string is enough for the one
		// constraint we care about and keeps deps minimal.
		if strings.Contains(err.Error(), "users_email_key") ||
			strings.Contains(err.Error(), "duplicate key") {
			return User{}, ErrEmailTaken
		}
		return User{}, err
	}
	return u, nil
}

// GetUserByEmail returns the user matching the given (lower-cased) email.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at
		   FROM users
		  WHERE email = $1`, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

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

// Instrument is a reusable voice preset. Config is the opaque JSON
// blob produced by the frontend synth editor (type, FM params, sample
// bytes base64, ADSR, etc.) — the backend never inspects its shape.
type Instrument struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

const instrumentCols = `id, name, config, updated_at`

func scanInstrument(row pgx.Row) (Instrument, error) {
	var i Instrument
	err := row.Scan(&i.ID, &i.Name, &i.Config, &i.UpdatedAt)
	return i, err
}

// ListInstruments returns every instrument preset owned by the user, newest first.
func (s *Store) ListInstruments(ctx context.Context, userID string) ([]Instrument, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+instrumentCols+`
		   FROM instruments
		  WHERE user_id = $1
		  ORDER BY updated_at DESC
		  LIMIT 500`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Instrument, 0, 16)
	for rows.Next() {
		it, err := scanInstrument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetInstrument returns a single instrument by id.
func (s *Store) GetInstrument(ctx context.Context, userID, id string) (Instrument, error) {
	it, err := scanInstrument(s.pool.QueryRow(ctx,
		`SELECT `+instrumentCols+`
		   FROM instruments
		  WHERE user_id = $1 AND id = $2`, userID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Instrument{}, ErrNotFound
	}
	return it, err
}

// CreateInstrument inserts a new instrument preset.
func (s *Store) CreateInstrument(ctx context.Context, userID string, in Instrument) (Instrument, error) {
	return scanInstrument(s.pool.QueryRow(ctx,
		`INSERT INTO instruments (user_id, name, config)
		      VALUES ($1, $2, $3)
		   RETURNING `+instrumentCols,
		userID, in.Name, in.Config))
}

// UpdateInstrument overwrites an existing preset owned by the user.
func (s *Store) UpdateInstrument(ctx context.Context, userID, id string, in Instrument) (Instrument, error) {
	res, err := scanInstrument(s.pool.QueryRow(ctx,
		`UPDATE instruments
		    SET name = $3, config = $4, updated_at = now()
		  WHERE user_id = $1 AND id = $2
		 RETURNING `+instrumentCols,
		userID, id, in.Name, in.Config))
	if errors.Is(err, pgx.ErrNoRows) {
		return Instrument{}, ErrNotFound
	}
	return res, err
}

// DeleteInstrument removes a preset owned by the user.
func (s *Store) DeleteInstrument(ctx context.Context, userID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM instruments WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
