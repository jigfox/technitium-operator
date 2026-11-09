// Package technitium is a minimal client for the Technitium DNS Server HTTP API.
package technitium

import (
	"context"
	"errors"
)

var (
	// ErrNotFound is returned when an installed app does not exist.
	ErrNotFound = errors.New("technitium: not found")
	// ErrAuth is returned when the login is refused.
	ErrAuth = errors.New("technitium: authentication failed")
)

// APIError is an error reported by Technitium in its JSON response.
type APIError struct {
	Message string
}

func (e *APIError) Error() string { return "technitium: " + e.Message }

// List selects one of the two domain lists.
type List string

const (
	// Allowed is the list of allowed zones.
	Allowed List = "allowed"
	// Blocked is the list of manually blocked zones.
	Blocked List = "blocked"
)

// Settings is the part of the server settings the operator reads.
type Settings struct {
	Version       string
	BlockListURLs []string
}

// Client is what the controllers need from Technitium.
type Client interface {
	// GetSettings returns the server version and managed settings.
	GetSettings(ctx context.Context) (Settings, error)
	// SetBlockListURLs replaces the block list URLs; an empty list clears them.
	SetBlockListURLs(ctx context.Context, urls []string) error

	// ListDomains returns the entries of a list, lowercase.
	ListDomains(ctx context.Context, list List) ([]string, error)
	// AddDomain adds an entry; adding an existing one is not an error.
	AddDomain(ctx context.Context, list List, domain string) error
	// DeleteDomain deletes an entry; deleting a missing one is not an error.
	DeleteDomain(ctx context.Context, list List, domain string) error

	// GetAppConfig returns the config of an installed app as JSON text (empty
	// if the app has none), or ErrNotFound if the app is not installed.
	GetAppConfig(ctx context.Context, app string) (string, error)
	// SetAppConfig replaces the config of an installed app (JSON text).
	SetAppConfig(ctx context.Context, app, config string) error

	// Close ends the session at the server.
	Close(ctx context.Context) error
}
