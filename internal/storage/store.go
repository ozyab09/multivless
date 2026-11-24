// Package storage persists servers, subscriptions, users and settings in SQLite.
package storage

import (
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/multivless/multivless/internal/subscriptions/parser"
)

//go:embed schema.sql
var schemaSQL string

// Store is the SQLite-backed persistence layer.
type Store struct {
	db *sql.DB
}

// Server is a persisted VLESS server plus operator metadata.
type Server struct {
	Info     *parser.Server
	SourceID string
	Enabled  bool
	Weight   int
	Created  time.Time
	Updated  time.Time
}

// Subscription is an imported subscription source.
type Subscription struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	URL                string    `json:"-"`
	IntervalSeconds    int       `json:"interval_seconds"`
	ServerCheckSeconds int       `json:"server_check_seconds"`
	Enabled            bool      `json:"enabled"`
	ETag               string    `json:"-"`
	LastModified       string    `json:"-"`
	LastCheckedAt      time.Time `json:"last_checked_at"`
	LastSuccessAt      time.Time `json:"last_success_at"`
	LastError          string    `json:"last_error"`
	ServerCount        int       `json:"server_count"`
	CreatedAt          time.Time `json:"created_at"`
}

// User is a SOCKS5 account.
type User struct {
	ID             string    `json:"id"`
	Username       string    `json:"username"`
	PasswordHash   string    `json:"-"`
	Enabled        bool      `json:"enabled"`
	MaxConnections int       `json:"max_connections"`
	BypassTorrent  bool      `json:"bypass_torrent"`
	CreatedAt      time.Time `json:"created_at"`
	LastLoginAt    time.Time `json:"last_login_at"`
	BytesIn        uint64    `json:"bytes_in"`
	BytesOut       uint64    `json:"bytes_out"`
}

// Open opens (and initialises) the database.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("storage path is empty")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}

	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Databases created before per-subscription health-check intervals need the
	// column added; SQLite has no ADD COLUMN IF NOT EXISTS.
	if _, err := db.Exec(`ALTER TABLE subscriptions ADD COLUMN server_check_seconds INTEGER NOT NULL DEFAULT 30`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		return nil, fmt.Errorf("migrate subscriptions: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN bypass_torrent INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		return nil, fmt.Errorf("migrate users: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("restrict database permissions: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// NewID returns a random identifier with a prefix.
func NewID(prefix string) string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return prefix + hex.EncodeToString(buf)
}

// ---- servers ----

// ListServers returns all persisted servers.
func (s *Store) ListServers() ([]Server, error) {
	rows, err := s.db.Query(`
		SELECT id, name, host, port, uuid, encryption, flow, packet_encoding,
		       security, sni, fingerprint, public_key, short_id, alpn, network,
		       host_header, path, service_name, mode, fragment, source_id,
		       weight, enabled, created_at, updated_at
		FROM servers ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("query servers: %w", err)
	}
	defer rows.Close()

	var out []Server
	for rows.Next() {
		var (
			srv      Server
			info     parser.Server
			alpn     string
			sourceID sql.NullString
			enabled  int
			created  string
			updated  string
		)
		if err := rows.Scan(
			&info.ID, &info.Name, &info.Host, &info.Port, &info.UUID,
			&info.Encryption, &info.Flow, &info.PacketEncoding,
			&info.Security, &info.SNI, &info.Fingerprint, &info.PublicKey, &info.ShortID, &alpn, &info.Network,
			&info.HostHeader, &info.Path, &info.ServiceName, &info.Mode, &info.Fragment, &sourceID,
			&srv.Weight, &enabled, &created, &updated,
		); err != nil {
			return nil, fmt.Errorf("scan server: %w", err)
		}
		if alpn != "" {
			info.ALPN = strings.Split(alpn, ",")
		}
		info.Weight = srv.Weight
		srv.Info = &info
		srv.SourceID = sourceID.String
		srv.Enabled = enabled == 1
		srv.Created = parseTime(created)
		srv.Updated = parseTime(updated)
		out = append(out, srv)
	}
	return out, rows.Err()
}

// UpsertServers inserts or updates servers in one transaction.
func (s *Store) UpsertServers(list []Server) error {
	if len(list) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO servers (
			id, name, host, port, uuid, encryption, flow, packet_encoding,
			security, sni, fingerprint, public_key, short_id, alpn, network,
			host_header, path, service_name, mode, fragment, source_id,
			weight, enabled, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, host=excluded.host, port=excluded.port,
			uuid=excluded.uuid, encryption=excluded.encryption, flow=excluded.flow,
			packet_encoding=excluded.packet_encoding, security=excluded.security,
			sni=excluded.sni, fingerprint=excluded.fingerprint, public_key=excluded.public_key,
			short_id=excluded.short_id, alpn=excluded.alpn, network=excluded.network,
			host_header=excluded.host_header, path=excluded.path,
			service_name=excluded.service_name, mode=excluded.mode,
			fragment=excluded.fragment, source_id=excluded.source_id,
			updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, srv := range list {
		info := srv.Info
		if info == nil {
			continue
		}
		var source any
		if srv.SourceID != "" {
			source = srv.SourceID
		}
		weight := srv.Weight
		if weight <= 0 {
			weight = info.Weight
		}
		if weight <= 0 {
			weight = 100
		}
		enabled := 0
		if srv.Enabled {
			enabled = 1
		}
		if _, err := stmt.Exec(
			info.ID, info.Name, info.Host, info.Port, info.UUID,
			info.Encryption, info.Flow, info.PacketEncoding,
			info.Security, info.SNI, info.Fingerprint, info.PublicKey, info.ShortID,
			strings.Join(info.ALPN, ","), info.Network, info.HostHeader, info.Path,
			info.ServiceName, info.Mode, info.Fragment, source,
			weight, enabled, now, now,
		); err != nil {
			return fmt.Errorf("upsert server %s: %w", info.ID, err)
		}
	}
	return tx.Commit()
}

// DeleteServer removes one server.
func (s *Store) DeleteServer(id string) error {
	_, err := s.db.Exec(`DELETE FROM servers WHERE id = ?`, id)
	return err
}

// DeleteSourceServersExcept removes servers of a subscription that are no
// longer present in its latest successful download.
func (s *Store) DeleteSourceServersExcept(sourceID string, keep []string) error {
	if sourceID == "" {
		return nil
	}
	query := `DELETE FROM servers WHERE source_id = ?`
	args := []any{sourceID}
	if len(keep) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
		query += ` AND id NOT IN (` + placeholders + `)`
		for _, id := range keep {
			args = append(args, id)
		}
	}
	_, err := s.db.Exec(query, args...)
	return err
}

// SetServerEnabled toggles a server.
func (s *Store) SetServerEnabled(id string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE servers SET enabled = ?, updated_at = ? WHERE id = ?`,
		v, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

// SetServerWeight updates the balancing weight.
func (s *Store) SetServerWeight(id string, weight int) error {
	_, err := s.db.Exec(`UPDATE servers SET weight = ?, updated_at = ? WHERE id = ?`,
		weight, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

// ---- subscriptions ----

// CreateSubscription stores a new subscription.
func (s *Store) CreateSubscription(sub Subscription) error {
	if sub.ServerCheckSeconds <= 0 {
		sub.ServerCheckSeconds = 30
	}
	_, err := s.db.Exec(`
		INSERT INTO subscriptions (id, name, url, interval_seconds, server_check_seconds, enabled, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		sub.ID, sub.Name, sub.URL, sub.IntervalSeconds, sub.ServerCheckSeconds, boolInt(sub.Enabled),
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// ListSubscriptions returns all subscriptions.
func (s *Store) ListSubscriptions() ([]Subscription, error) {
	rows, err := s.db.Query(`
		SELECT id, name, url, interval_seconds, server_check_seconds, enabled, etag, last_modified,
		       last_checked_at, last_success_at, last_error, server_count, created_at
		FROM subscriptions ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Subscription
	for rows.Next() {
		var (
			sub              Subscription
			enabled          int
			checked, success string
			created          string
		)
		if err := rows.Scan(&sub.ID, &sub.Name, &sub.URL, &sub.IntervalSeconds, &sub.ServerCheckSeconds, &enabled,
			&sub.ETag, &sub.LastModified, &checked, &success, &sub.LastError,
			&sub.ServerCount, &created); err != nil {
			return nil, err
		}
		sub.Enabled = enabled == 1
		sub.LastCheckedAt = parseTime(checked)
		sub.LastSuccessAt = parseTime(success)
		sub.CreatedAt = parseTime(created)
		out = append(out, sub)
	}
	return out, rows.Err()
}

// GetSubscription loads one subscription.
func (s *Store) GetSubscription(id string) (Subscription, error) {
	var (
		sub              Subscription
		enabled          int
		checked, success string
		created          string
	)
	err := s.db.QueryRow(`
		SELECT id, name, url, interval_seconds, server_check_seconds, enabled, etag, last_modified,
		       last_checked_at, last_success_at, last_error, server_count, created_at
		FROM subscriptions WHERE id = ?`, id).
		Scan(&sub.ID, &sub.Name, &sub.URL, &sub.IntervalSeconds, &sub.ServerCheckSeconds, &enabled,
			&sub.ETag, &sub.LastModified, &checked, &success, &sub.LastError,
			&sub.ServerCount, &created)
	if err != nil {
		return Subscription{}, err
	}
	sub.Enabled = enabled == 1
	sub.LastCheckedAt = parseTime(checked)
	sub.LastSuccessAt = parseTime(success)
	sub.CreatedAt = parseTime(created)
	return sub, nil
}

// UpdateSubscription updates editable fields.
func (s *Store) UpdateSubscription(id, name, rawURL string, interval, serverCheck int, enabled bool) error {
	if serverCheck <= 0 {
		serverCheck = 30
	}
	_, err := s.db.Exec(`UPDATE subscriptions
		SET name = ?, url = ?, interval_seconds = ?, server_check_seconds = ?, enabled = ?
		WHERE id = ?`, name, rawURL, interval, serverCheck, boolInt(enabled), id)
	return err
}

// DeleteSubscription removes a subscription and its servers.
func (s *Store) DeleteSubscription(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM servers WHERE source_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM subscriptions WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordSubscriptionResult stores the outcome of an update attempt.
func (s *Store) RecordSubscriptionResult(id, etag, lastModified, errMsg string, serverCount int, success bool) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if success {
		_, err := s.db.Exec(`
			UPDATE subscriptions
			SET etag = ?, last_modified = ?, last_checked_at = ?, last_success_at = ?,
			    last_error = '', server_count = ?
			WHERE id = ?`, etag, lastModified, now, now, serverCount, id)
		return err
	}
	_, err := s.db.Exec(`
		UPDATE subscriptions SET last_checked_at = ?, last_error = ? WHERE id = ?`,
		now, errMsg, id)
	return err
}

// ---- settings ----

// GetSetting loads a setting.
func (s *Store) GetSetting(key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting writes a setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func parseTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}
