package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// DefaultUsername and DefaultPassword are the bootstrap credentials created on
// first boot. Changeable from the Settings screen.
const (
	DefaultUsername = "doot"
	DefaultPassword = "doot"
)

// User is the single operator of this instance.
type User struct {
	ID                     string
	Username               string
	PasswordHash           string
	PasswordChangeRequired bool
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// CheckPassword reports whether the supplied password matches.
func (u *User) CheckPassword(password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil
}

// HashPassword hashes a plaintext password for storage.
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

const userColumns = `id, username, password_hash, password_change_required, created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordChangeRequired, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	return &u, nil
}

// UserByUsername looks up a user for login.
func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.DB.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE username = $1`, username))
}

// UserByID looks up a user from a session cookie.
func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.DB.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

// UpdateCredentials changes the username and password hash.
//
// clearChangeRequired should be true only when the password actually changed:
// renaming the account while still on the default password must keep the marker.
func (s *Store) UpdateCredentials(ctx context.Context, id, username, passwordHash string, clearChangeRequired bool) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE users
		   SET username = $2,
		       password_hash = $3,
		       password_change_required = password_change_required AND NOT $4,
		       updated_at = now()
		 WHERE id = $1`, id, username, passwordHash, clearChangeRequired)
	if err != nil {
		return fmt.Errorf("update credentials: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// BootstrapUser creates the default user when no user exists.
//
// The check is for zero users rather than for a user named "doot" on purpose:
// once the account has been renamed, an existence check on "doot" would
// helpfully recreate it on every deploy.
func (s *Store) BootstrapUser(ctx context.Context, log *slog.Logger) error {
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return nil
	}

	hash, err := HashPassword(DefaultPassword)
	if err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO users (username, password_hash, password_change_required)
		VALUES ($1, $2, true)
		ON CONFLICT (username) DO NOTHING`, DefaultUsername, hash); err != nil {
		return fmt.Errorf("create default user: %w", err)
	}

	log.Warn("created default user - change these credentials in Settings",
		"username", DefaultUsername, "password", DefaultPassword)
	return nil
}
