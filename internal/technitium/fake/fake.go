// Package fake is an in-memory technitium.Client for tests.
package fake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jigfox/technitium-operator/internal/technitium"
)

// Client is an in-memory Technitium. Mutations are recorded in Calls.
type Client struct {
	mu            sync.Mutex
	version       string
	lists         map[technitium.List]map[string]bool
	blockListURLs []string
	apps          map[string]string // installed app -> config JSON text
	calls         []string
	closed        bool
	// Err, if set, is returned by every method (to simulate an outage).
	Err error
}

var _ technitium.Client = (*Client)(nil)

// New returns an empty server of version 15.0.
func New() *Client {
	return &Client{
		version: "15.0",
		lists:   map[technitium.List]map[string]bool{technitium.Allowed: {}, technitium.Blocked: {}},
		apps:    map[string]string{},
	}
}

// InstallApp makes an app known, with its config as JSON text.
func (c *Client) InstallApp(name, config string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apps[name] = config
}

// Seed adds entries without recording calls.
func (c *Client) Seed(list technitium.List, domains ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range domains {
		c.lists[list][strings.ToLower(d)] = true
	}
}

// SeedBlockListURLs sets the URLs without recording a call.
func (c *Client) SeedBlockListURLs(urls ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockListURLs = slices.Clone(urls)
}

// Domains returns the sorted entries of a list.
func (c *Client) Domains(list technitium.List) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sorted(list)
}

func (c *Client) sorted(list technitium.List) []string {
	out := make([]string, 0, len(c.lists[list]))
	for d := range c.lists[list] {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// BlockListURLs returns the current URLs.
func (c *Client) BlockListURLs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.blockListURLs)
}

// AppConfig returns the stored config text of an app.
func (c *Client) AppConfig(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apps[name]
}

// Calls returns the recorded mutations, e.g. "add allowed example.com".
func (c *Client) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// ResetCalls forgets the recorded mutations.
func (c *Client) ResetCalls() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = nil
}

// Closed reports whether Close was called.
func (c *Client) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// GetSettings implements technitium.Client.
func (c *Client) GetSettings(context.Context) (technitium.Settings, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return technitium.Settings{}, c.Err
	}
	return technitium.Settings{Version: c.version, BlockListURLs: slices.Clone(c.blockListURLs)}, nil
}

// SetBlockListURLs implements technitium.Client.
func (c *Client) SetBlockListURLs(_ context.Context, urls []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return c.Err
	}
	c.calls = append(c.calls, "set blocklisturls "+strings.Join(urls, ","))
	c.blockListURLs = slices.Clone(urls)
	return nil
}

// ListDomains implements technitium.Client.
func (c *Client) ListDomains(_ context.Context, list technitium.List) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return nil, c.Err
	}
	return c.sorted(list), nil
}

// AddDomain implements technitium.Client.
func (c *Client) AddDomain(_ context.Context, list technitium.List, domain string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return c.Err
	}
	c.calls = append(c.calls, fmt.Sprintf("add %s %s", list, domain))
	c.lists[list][strings.ToLower(domain)] = true
	return nil
}

// DeleteDomain implements technitium.Client.
func (c *Client) DeleteDomain(_ context.Context, list technitium.List, domain string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return c.Err
	}
	c.calls = append(c.calls, fmt.Sprintf("delete %s %s", list, domain))
	delete(c.lists[list], strings.ToLower(domain))
	return nil
}

// GetAppConfig implements technitium.Client.
func (c *Client) GetAppConfig(_ context.Context, app string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return "", c.Err
	}
	cfg, ok := c.apps[app]
	if !ok {
		return "", fmt.Errorf("%w: DNS application was not found: %s", technitium.ErrNotFound, app)
	}
	return cfg, nil
}

// SetAppConfig implements technitium.Client.
func (c *Client) SetAppConfig(_ context.Context, app, config string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return c.Err
	}
	if _, ok := c.apps[app]; !ok {
		return fmt.Errorf("%w: DNS application was not found: %s", technitium.ErrNotFound, app)
	}
	c.calls = append(c.calls, "set appconfig "+app)
	c.apps[app] = config
	return nil
}

// Close implements technitium.Client.
func (c *Client) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
