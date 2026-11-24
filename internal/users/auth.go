// Package users authenticates SOCKS5 clients against the database.
package users

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/multivless/multivless/internal/config"
	"github.com/multivless/multivless/internal/security"
	"github.com/multivless/multivless/internal/socks5"
	"github.com/multivless/multivless/internal/storage"
)

// Errors returned to clients. Messages stay generic on purpose.
var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrDisabled           = errors.New("account disabled")
)

// Authenticator implements socks5.Authenticator on top of SQLite.
type Authenticator struct {
	store  *storage.Store
	logger *slog.Logger
}

// NewAuthenticator creates an authenticator.
func NewAuthenticator(store *storage.Store, logger *slog.Logger) *Authenticator {
	return &Authenticator{store: store, logger: logger}
}

// Authenticate verifies username/password and returns the principal.
func (a *Authenticator) Authenticate(username, password string) (*socks5.Principal, error) {
	user, err := a.store.GetUserByUsername(username)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	if !user.Enabled {
		return nil, ErrDisabled
	}
	if !security.VerifyPassword(user.PasswordHash, password) {
		return nil, ErrInvalidCredentials
	}
	if err := a.store.RecordLogin(user.ID); err != nil {
		a.logger.Debug("cannot record login", "user", username, "error", err)
	}
	return &socks5.Principal{
		ID:             user.ID,
		Username:       user.Username,
		MaxConnections: user.MaxConnections,
		BypassTorrent:  user.BypassTorrent,
	}, nil
}

// SeedFromConfig creates accounts from the YAML users list on first start.
func SeedFromConfig(store *storage.Store, users []config.User, logger *slog.Logger) error {
	if len(users) == 0 {
		return nil
	}
	count, err := store.CountUsers()
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return nil
	}
	for _, u := range users {
		hash, err := security.HashPassword(u.Password)
		if err != nil {
			return err
		}
		if err := store.CreateUser(storage.User{
			ID:           storage.NewID("usr_"),
			Username:     u.Username,
			PasswordHash: hash,
			Enabled:      true,
		}); err != nil {
			return err
		}
		logger.Info("seeded SOCKS5 user from config", "username", u.Username)
	}
	return nil
}
