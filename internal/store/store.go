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

// GetUserByID returns the user with the given id (no email lookup —
// used by the share handlers to format the invitation email).
func (s *Store) GetUserByID(ctx context.Context, id string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at
		   FROM users
		  WHERE id = $1`, id).
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

// GetShared returns a pattern accessible to the user (owns it or
// reaches it through a song share).
func (s *Store) GetShared(ctx context.Context, userID, id string) (Pattern, error) {
	role, err := s.PatternAccess(ctx, userID, id)
	if err != nil {
		return Pattern{}, err
	}
	if role == "" {
		return Pattern{}, ErrNotFound
	}
	p, err := scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM patterns WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Pattern{}, ErrNotFound
	}
	return p, err
}

// UpdateShared is the editor-aware variant — allows non-owners with
// editor access (through a song share) to overwrite the pattern.
func (s *Store) UpdateShared(ctx context.Context, userID, id string, p Pattern) (Pattern, error) {
	role, err := s.PatternAccess(ctx, userID, id)
	if err != nil {
		return Pattern{}, err
	}
	if role == "" {
		return Pattern{}, ErrNotFound
	}
	// (No "viewer" role exists for patterns yet; the song share only
	// grants 'editor' — if we add view-only later, refuse here.)
	res, err := scan(s.pool.QueryRow(ctx,
		`UPDATE patterns
		    SET name = $2, bpm = $3, swing = $4, steps = $5, updated_at = now()
		  WHERE id = $1
		 RETURNING `+columns,
		id, p.Name, p.BPM, p.Swing, p.Steps))
	if errors.Is(err, pgx.ErrNoRows) {
		return Pattern{}, ErrNotFound
	}
	return res, err
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

// GetSongUnchecked returns a song without filtering by user_id. Used by
// the share-preview endpoint which needs the song name before the
// invitee logs in.
func (s *Store) GetSongUnchecked(ctx context.Context, id string) (Song, error) {
	song, err := scanSong(s.pool.QueryRow(ctx,
		`SELECT `+songCols+` FROM songs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Song{}, ErrNotFound
	}
	return song, err
}

// GetSongShared returns a song the user can access (owns or has been
// shared and accepted). Used by GetSong handler when the caller may be
// a collaborator rather than the owner.
func (s *Store) GetSongShared(ctx context.Context, userID, id string) (Song, error) {
	song, err := scanSong(s.pool.QueryRow(ctx,
		`SELECT `+songCols+` FROM songs s
		  WHERE s.id = $1 AND (
		    s.user_id = $2
		    OR EXISTS (SELECT 1 FROM shares sh
		                WHERE sh.resource_type='song'
		                  AND sh.resource_id=s.id
		                  AND sh.invitee_id=$2
		                  AND sh.accepted_at IS NOT NULL)
		  )`, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Song{}, ErrNotFound
	}
	return song, err
}

// UpdateSongShared is the editor-aware variant of UpdateSong — allows
// non-owner editors to update a song through their share row.
func (s *Store) UpdateSongShared(ctx context.Context, userID, id string, in Song) (Song, error) {
	role, err := s.SongAccess(ctx, userID, id)
	if err != nil {
		return Song{}, err
	}
	if role == "" || role == "viewer" {
		return Song{}, ErrNotFound
	}
	// Bypass the user_id filter — access is checked above.
	res, err := scanSong(s.pool.QueryRow(ctx,
		`UPDATE songs
		    SET name = $2, items = $3, updated_at = now()
		  WHERE id = $1
		 RETURNING `+songCols,
		id, in.Name, in.Items))
	if errors.Is(err, pgx.ErrNoRows) {
		return Song{}, ErrNotFound
	}
	return res, err
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

// ----- Shares (collaborative access to songs) ------------------------

// Share grants a non-owner user the right to view/edit a resource.
// invitee_id is null until the invitation is accepted (i.e. the
// invited email matches a real account).
type Share struct {
	ID            string     `json:"id"`
	ResourceType  string     `json:"resourceType"`
	ResourceID    string     `json:"resourceId"`
	OwnerID       string     `json:"ownerId"`
	OwnerEmail    string     `json:"ownerEmail,omitempty"`
	InviteeID     *string    `json:"inviteeId,omitempty"`
	InviteeEmail  string     `json:"inviteeEmail"`
	Role          string     `json:"role"`
	Token         string     `json:"token,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	AcceptedAt    *time.Time `json:"acceptedAt,omitempty"`
}

// ErrShareExists is returned when the same (resource, email) is invited twice.
var ErrShareExists = errors.New("invitation already exists")

// CreateShare records the invitation. If a user with the given email
// exists we link invitee_id immediately so the friend sees the resource
// the next time they list it (the email step is then just a courtesy).
func (s *Store) CreateShare(ctx context.Context,
	resourceType, resourceID, ownerID, inviteeEmail, role, token string) (Share, error) {

	var sh Share
	// Try to resolve the email to an existing account. We don't care if
	// it doesn't exist — the row stays pending until the user signs up.
	var inviteeID *string
	row := s.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE lower(email) = lower($1)`, inviteeEmail)
	var uid string
	if err := row.Scan(&uid); err == nil {
		inviteeID = &uid
	}

	// Cast $4 explicitly so Postgres can resolve the type when nil is
	// passed (CASE arg type inference fails otherwise — 42P08).
	err := s.pool.QueryRow(ctx,
		`INSERT INTO shares
		    (resource_type, resource_id, owner_id, invitee_id, invitee_email, role, token,
		     accepted_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7,
		         CASE WHEN $4::uuid IS NULL THEN NULL ELSE now() END)
		 RETURNING id, resource_type, resource_id::text, owner_id, invitee_id,
		           invitee_email, role, token, created_at, accepted_at`,
		resourceType, resourceID, ownerID, inviteeID, inviteeEmail, role, token).
		Scan(&sh.ID, &sh.ResourceType, &sh.ResourceID, &sh.OwnerID, &sh.InviteeID,
			&sh.InviteeEmail, &sh.Role, &sh.Token, &sh.CreatedAt, &sh.AcceptedAt)
	if err != nil {
		if strings.Contains(err.Error(), "shares_resource_invitee_unique") ||
			strings.Contains(err.Error(), "duplicate key") {
			return Share{}, ErrShareExists
		}
		return Share{}, err
	}
	return sh, nil
}

// ListSharesForResource lists every share emitted by `ownerID` on a
// given resource. Used by the inviter UI.
func (s *Store) ListSharesForResource(ctx context.Context,
	ownerID, resourceType, resourceID string) ([]Share, error) {

	rows, err := s.pool.Query(ctx,
		`SELECT id, resource_type, resource_id::text, owner_id, invitee_id,
		        invitee_email, role, token, created_at, accepted_at
		   FROM shares
		  WHERE owner_id = $1 AND resource_type = $2 AND resource_id = $3
		  ORDER BY created_at DESC`,
		ownerID, resourceType, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Share, 0, 4)
	for rows.Next() {
		var sh Share
		if err := rows.Scan(&sh.ID, &sh.ResourceType, &sh.ResourceID, &sh.OwnerID,
			&sh.InviteeID, &sh.InviteeEmail, &sh.Role, &sh.Token,
			&sh.CreatedAt, &sh.AcceptedAt); err != nil {
			return nil, err
		}
		// Don't leak tokens for already-accepted shares (they're useless anyway).
		if sh.AcceptedAt != nil {
			sh.Token = ""
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// DeleteShare removes an invitation owned by `ownerID`. Used to revoke.
func (s *Store) DeleteShare(ctx context.Context, ownerID, shareID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM shares WHERE id = $1 AND owner_id = $2`, shareID, ownerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetShareByToken returns a share by its public token. Used by the
// accept flow.
func (s *Store) GetShareByToken(ctx context.Context, token string) (Share, error) {
	var sh Share
	err := s.pool.QueryRow(ctx,
		`SELECT s.id, s.resource_type, s.resource_id::text, s.owner_id,
		        u.email, s.invitee_id, s.invitee_email, s.role,
		        s.token, s.created_at, s.accepted_at
		   FROM shares s
		   JOIN users u ON u.id = s.owner_id
		  WHERE s.token = $1`, token).
		Scan(&sh.ID, &sh.ResourceType, &sh.ResourceID, &sh.OwnerID,
			&sh.OwnerEmail, &sh.InviteeID, &sh.InviteeEmail, &sh.Role,
			&sh.Token, &sh.CreatedAt, &sh.AcceptedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Share{}, ErrNotFound
	}
	return sh, err
}

// AcceptShare binds a share to a concrete user and marks it accepted.
// If the share already has an invitee_id different from userID, we
// refuse — token belongs to someone else.
func (s *Store) AcceptShare(ctx context.Context, token, userID, userEmail string) (Share, error) {
	sh, err := s.GetShareByToken(ctx, token)
	if err != nil {
		return Share{}, err
	}
	if sh.InviteeID != nil && *sh.InviteeID != userID {
		return Share{}, ErrNotFound // refuse silently to avoid token probing
	}
	// Allow accepting from any logged-in account; we update both id + email
	// so the row reflects who actually claimed it.
	err = s.pool.QueryRow(ctx,
		`UPDATE shares
		    SET invitee_id = $2, invitee_email = $3,
		        accepted_at = COALESCE(accepted_at, now())
		  WHERE id = $1
		 RETURNING id, resource_type, resource_id::text, owner_id, invitee_id,
		           invitee_email, role, token, created_at, accepted_at`,
		sh.ID, userID, userEmail).
		Scan(&sh.ID, &sh.ResourceType, &sh.ResourceID, &sh.OwnerID, &sh.InviteeID,
			&sh.InviteeEmail, &sh.Role, &sh.Token, &sh.CreatedAt, &sh.AcceptedAt)
	return sh, err
}

// AcceptPendingSharesForEmail is called when a user signs up or logs in:
// every pending share (no invitee_id) whose email matches gets bound to
// the user. The user is then opted-in transparently.
func (s *Store) AcceptPendingSharesForEmail(ctx context.Context, userID, email string) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE shares
		    SET invitee_id = $1, accepted_at = COALESCE(accepted_at, now())
		  WHERE invitee_id IS NULL AND lower(invitee_email) = lower($2)`,
		userID, email)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Helpers used by the listing endpoints --------------------------------

// AccessibleSongs returns the union of songs the user owns and songs
// that have been shared (and accepted) with them.
func (s *Store) AccessibleSongs(ctx context.Context, userID string) ([]Song, error) {
	rows, err := s.pool.Query(ctx,
		`(SELECT `+songCols+` FROM songs WHERE user_id = $1)
		 UNION
		 (SELECT s.id, s.name, s.items, s.updated_at FROM songs s
		    JOIN shares sh ON sh.resource_type = 'song'
		                 AND sh.resource_id = s.id
		                 AND sh.invitee_id = $1
		                 AND sh.accepted_at IS NOT NULL)
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

// SongAccess returns the role the user has on the song, or empty
// string if they cannot see it at all.
func (s *Store) SongAccess(ctx context.Context, userID, songID string) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx,
		`SELECT CASE
		          WHEN EXISTS (SELECT 1 FROM songs
		                       WHERE id = $1 AND user_id = $2)
		            THEN 'owner'
		          ELSE COALESCE((SELECT role FROM shares
		                          WHERE resource_type = 'song'
		                            AND resource_id = $1
		                            AND invitee_id = $2
		                            AND accepted_at IS NOT NULL), '')
		        END`, songID, userID).Scan(&role)
	return role, err
}

// AccessiblePatterns returns the union of patterns the user owns and
// patterns referenced by songs the user has accepted-shared access to.
// The references live in songs.items[].patternId (jsonb).
func (s *Store) AccessiblePatterns(ctx context.Context, userID string) ([]Pattern, error) {
	rows, err := s.pool.Query(ctx,
		`(SELECT `+columns+` FROM patterns WHERE user_id = $1)
		 UNION
		 (SELECT `+columns+` FROM patterns p
		    WHERE p.id::text IN (
		      SELECT item->>'patternId'
		        FROM songs s
		        JOIN shares sh ON sh.resource_type = 'song'
		                     AND sh.resource_id = s.id
		                     AND sh.invitee_id = $1
		                     AND sh.accepted_at IS NOT NULL,
		             jsonb_array_elements(s.items) item
		    ))
		 ORDER BY updated_at DESC
		 LIMIT 400`, userID)
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

// PatternAccess: owner | shared-via-song | empty.
func (s *Store) PatternAccess(ctx context.Context, userID, patternID string) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx,
		`SELECT CASE
		    WHEN EXISTS (SELECT 1 FROM patterns
		                  WHERE id = $1 AND user_id = $2)
		      THEN 'owner'
		    WHEN EXISTS (
		      SELECT 1 FROM songs s
		        JOIN shares sh ON sh.resource_type = 'song'
		                     AND sh.resource_id = s.id
		                     AND sh.invitee_id = $2
		                     AND sh.accepted_at IS NOT NULL,
		           jsonb_array_elements(s.items) item
		       WHERE item->>'patternId' = $1::text)
		      THEN 'editor'
		    ELSE ''
		  END`, patternID, userID).Scan(&role)
	return role, err
}
