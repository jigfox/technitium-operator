package technitium

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Credentials is how the client authenticates: an API token, or a username
// and password for which it keeps a session. The token wins if both are set.
type Credentials struct {
	Username string
	Password string
	Token    string
}

// HTTPClient talks to the Technitium web service. Every call is a POST with
// form parameters and a bearer token in the Authorization header. With a
// username and password it logs in lazily, again when Technitium answers
// invalid-token (the session expired), and logs out in Close.
type HTTPClient struct {
	baseURL string
	cred    Credentials
	http    *http.Client

	mu      sync.Mutex
	session string // session token from /api/user/login
}

var _ Client = (*HTTPClient)(nil)

// NewHTTPClient returns a client for the server at baseURL, for example
// http://technitium-0.dns.svc:5380. A nil httpClient uses a default with a
// timeout.
func NewHTTPClient(baseURL string, cred Credentials, httpClient *http.Client) *HTTPClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), cred: cred, http: httpClient}
}

// envelope is the common part of every JSON response.
type envelope struct {
	Status       string          `json:"status"`
	ErrorMessage string          `json:"errorMessage"`
	Response     json.RawMessage `json:"response"`
}

const (
	statusOK           = "ok"
	statusInvalidToken = "invalid-token"

	maxBody = 64 << 20
)

// errInvalidToken is internal: the session must be renewed.
var errInvalidToken = errors.New("technitium: invalid or expired token")

func (c *HTTPClient) usesAPIToken() bool { return c.cred.Token != "" }

// bearer returns the token to send, logging in if needed.
func (c *HTTPClient) bearer(ctx context.Context, force bool) (string, error) {
	if c.usesAPIToken() {
		return c.cred.Token, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != "" && !force {
		return c.session, nil
	}
	body, err := c.post(ctx, "/api/user/login", url.Values{"user": {c.cred.Username}, "pass": {c.cred.Password}}, "")
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body.raw, &out); err != nil || out.Token == "" {
		return "", errors.New("technitium: login response has no token")
	}
	c.session = out.Token
	return c.session, nil
}

type reply struct {
	// raw is the whole body.
	raw []byte
	// env is set if the body is a JSON envelope.
	env *envelope
}

// post sends one request. It returns errors for HTTP failures and for
// Technitium's error statuses; a body that is not a JSON object (the domain
// list export is plain text) is returned as is.
func (c *HTTPClient) post(ctx context.Context, path string, form url.Values, token string) (*reply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("technitium request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("technitium response: %w", err)
	}
	rep := &reply{raw: data}
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '{' {
		var env envelope
		if err := json.Unmarshal(trimmed, &env); err != nil {
			return nil, fmt.Errorf("technitium: unexpected response (status %d)", resp.StatusCode)
		}
		rep.env = &env
		switch env.Status {
		case statusOK:
		case statusInvalidToken:
			return nil, errInvalidToken
		default:
			return nil, apiError(env)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("technitium: unexpected status %d", resp.StatusCode)
	}
	return rep, nil
}

func apiError(env envelope) error {
	msg := env.ErrorMessage
	if msg == "" {
		msg = "request failed with status " + env.Status
	}
	switch {
	case strings.HasPrefix(msg, "Invalid username or password"):
		return fmt.Errorf("%w: %s", ErrAuth, msg)
	case strings.Contains(msg, "was not found"):
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	}
	return &APIError{Message: msg}
}

// call runs an authenticated request, renewing the session once if needed.
func (c *HTTPClient) call(ctx context.Context, path string, form url.Values) (*reply, error) {
	token, err := c.bearer(ctx, false)
	if err != nil {
		return nil, err
	}
	rep, err := c.post(ctx, path, form, token)
	if errors.Is(err, errInvalidToken) {
		if c.usesAPIToken() {
			return nil, fmt.Errorf("%w: the API token was refused", ErrAuth)
		}
		if token, err = c.bearer(ctx, true); err != nil {
			return nil, err
		}
		rep, err = c.post(ctx, path, form, token)
		if errors.Is(err, errInvalidToken) {
			return nil, fmt.Errorf("%w: the new session was refused", ErrAuth)
		}
	}
	return rep, err
}

// result returns the "response" object of an envelope reply.
func (r *reply) result() json.RawMessage {
	if r.env == nil {
		return nil
	}
	return r.env.Response
}

// GetSettings implements Client.
func (c *HTTPClient) GetSettings(ctx context.Context) (Settings, error) {
	rep, err := c.call(ctx, "/api/settings/get", nil)
	if err != nil {
		return Settings{}, err
	}
	var out struct {
		Version       string   `json:"version"`
		BlockListURLs []string `json:"blockListUrls"`
	}
	if err := json.Unmarshal(rep.result(), &out); err != nil {
		return Settings{}, fmt.Errorf("technitium: unexpected settings response: %w", err)
	}
	return Settings{Version: out.Version, BlockListURLs: out.BlockListURLs}, nil
}

// SetBlockListURLs implements Client. Technitium takes a comma separated
// list, and the word "false" to clear it.
func (c *HTTPClient) SetBlockListURLs(ctx context.Context, urls []string) error {
	value := "false"
	if len(urls) > 0 {
		if slices.ContainsFunc(urls, func(u string) bool { return u == "" || strings.Contains(u, ",") }) {
			return errors.New("technitium: block list URLs must not be empty or contain a comma")
		}
		value = strings.Join(urls, ",")
	}
	_, err := c.call(ctx, "/api/settings/set", url.Values{"blockListUrls": {value}})
	return err
}

// ListDomains implements Client.
func (c *HTTPClient) ListDomains(ctx context.Context, list List) ([]string, error) {
	rep, err := c.call(ctx, "/api/"+string(list)+"/export", nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(string(rep.raw), "\n") {
		if d := strings.ToLower(strings.TrimSpace(line)); d != "" {
			out = append(out, d)
		}
	}
	return out, nil
}

// AddDomain implements Client.
func (c *HTTPClient) AddDomain(ctx context.Context, list List, domain string) error {
	_, err := c.call(ctx, "/api/"+string(list)+"/add", url.Values{"domain": {domain}})
	return err
}

// DeleteDomain implements Client.
func (c *HTTPClient) DeleteDomain(ctx context.Context, list List, domain string) error {
	_, err := c.call(ctx, "/api/"+string(list)+"/delete", url.Values{"domain": {domain}})
	return err
}

// GetAppConfig implements Client.
func (c *HTTPClient) GetAppConfig(ctx context.Context, app string) (string, error) {
	rep, err := c.call(ctx, "/api/apps/config/get", url.Values{"name": {app}})
	if err != nil {
		return "", err
	}
	var out struct {
		Config *string `json:"config"`
	}
	if err := json.Unmarshal(rep.result(), &out); err != nil {
		return "", fmt.Errorf("technitium: unexpected app config response: %w", err)
	}
	if out.Config == nil {
		return "", nil
	}
	return *out.Config, nil
}

// SetAppConfig implements Client.
func (c *HTTPClient) SetAppConfig(ctx context.Context, app, config string) error {
	_, err := c.call(ctx, "/api/apps/config/set", url.Values{"name": {app}, "config": {config}})
	return err
}

// Close implements Client. A session that cannot be ended is left to expire.
func (c *HTTPClient) Close(ctx context.Context) error {
	if c.usesAPIToken() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == "" {
		return nil
	}
	_, err := c.post(ctx, "/api/user/logout", nil, c.session)
	c.session = ""
	if errors.Is(err, errInvalidToken) {
		return nil
	}
	return err
}
