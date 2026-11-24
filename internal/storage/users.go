package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

const userColumns = `id, username, password_hash, enabled, max_connections, bypass_torrent, created_at, last_login_at, bytes_in, bytes_out`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var (
		u        User
		enabled  int
		bypass   int
		created  string
		lastSeen string
	)
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &enabled, &u.MaxConnections, &bypass,
		&created, &lastSeen, &u.BytesIn, &u.BytesOut)
	if err != nil {
		return User{}, err
	}
	u.Enabled = enabled == 1
	u.BypassTorrent = bypass == 1
	u.CreatedAt = parseTime(created)
	u.LastLoginAt = parseTime(lastSeen)
	return u, nil
}

// ListUsers returns all SOCKS5 accounts.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + ` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUser loads one account by ID.
func (s *Store) GetUser(id string) (User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// GetUserByUsername loads one account by username.
func (s *Store) GetUserByUsername(username string) (User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE username = ?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// CountUsers returns the number of accounts.
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts an account.
func (s *Store) CreateUser(u User) error {
	_, err := s.db.Exec(`
		INSERT INTO users (id, username, password_hash, enabled, max_connections, bypass_torrent, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		u.ID, u.Username, u.PasswordHash, boolInt(u.Enabled), u.MaxConnections, boolInt(u.BypassTorrent),
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// SetUserPassword replaces the password hash.
func (s *Store) SetUserPassword(id, hash string) error {
	_, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
	return err
}

// SetUserEnabled enables or disables an account.
func (s *Store) SetUserEnabled(id string, enabled bool) error {
	_, err := s.db.Exec(`UPDATE users SET enabled = ? WHERE id = ?`, boolInt(enabled), id)
	return err
}

// SetUserTorrentBypass routes BitTorrent traffic directly for this user.
func (s *Store) SetUserTorrentBypass(id string, bypass bool) error {
	_, err := s.db.Exec(`UPDATE users SET bypass_torrent = ? WHERE id = ?`, boolInt(bypass), id)
	return err
}

// SetUserMaxConnections updates the per-user connection cap (0 = unlimited).
func (s *Store) SetUserMaxConnections(id string, limit int) error {
	_, err := s.db.Exec(`UPDATE users SET max_connections = ? WHERE id = ?`, limit, id)
	return err
}

// DeleteUser removes an account.
func (s *Store) DeleteUser(id string) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// RecordLogin updates the last-login timestamp.
func (s *Store) RecordLogin(id string) error {
	_, err := s.db.Exec(`UPDATE users SET last_login_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

// AddUserBytes accumulates traffic counters.
func (s *Store) AddUserBytes(id string, in, out uint64) error {
	_, err := s.db.Exec(`UPDATE users SET bytes_in = bytes_in + ?, bytes_out = bytes_out + ? WHERE id = ?`,
		in, out, id)
	return err
}
