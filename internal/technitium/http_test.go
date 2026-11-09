package technitium_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jigfox/technitium-operator/internal/technitium"
)

// server is a scripted Technitium: handlers by path, requests recorded.
type server struct {
	t        *testing.T
	mu       sync.Mutex
	handlers map[string]func(w http.ResponseWriter, form url.Values)
	requests []request
	*httptest.Server
}

type request struct {
	Path, Auth, Method string
	Form               url.Values
}

func newServer(t *testing.T) *server {
	s := &server{t: t, handlers: map[string]func(http.ResponseWriter, url.Values){}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content type = %q", ct)
		}
		s.mu.Lock()
		s.requests = append(s.requests, request{r.URL.Path, r.Header.Get("Authorization"), r.Method, form})
		h := s.handlers[r.URL.Path]
		s.mu.Unlock()
		if h == nil {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		h(w, form)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) handle(path string, h func(w http.ResponseWriter, form url.Values)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[path] = h
}

func (s *server) reqs(path string) []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []request
	for _, r := range s.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func reply(body string) func(http.ResponseWriter, url.Values) {
	return func(w http.ResponseWriter, _ url.Values) { _, _ = io.WriteString(w, body) }
}

const okEmpty = `{"response":{},"status":"ok"}`

func passwordLogin(s *server, tokens ...string) {
	i := 0
	s.handle("/api/user/login", func(w http.ResponseWriter, form url.Values) {
		if form.Get("user") != "admin" || form.Get("pass") != "secret" {
			_, _ = io.WriteString(w, `{"status":"error","errorMessage":"Invalid username or password for user: admin"}`)
			return
		}
		tok := tokens[min(i, len(tokens)-1)]
		i++
		_, _ = io.WriteString(w, `{"username":"admin","token":"`+tok+`","status":"ok"}`)
	})
}

var admin = technitium.Credentials{Username: "admin", Password: "secret"}

func TestLoginAndBearer(t *testing.T) {
	s := newServer(t)
	passwordLogin(s, "tok1")
	s.handle("/api/allowed/export", reply("Example.com\nfoo.org\n\n"))
	c := technitium.NewHTTPClient(s.URL+"/", admin, nil)

	got, err := c.ListDomains(context.Background(), technitium.Allowed)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"example.com", "foo.org"}; !slices.Equal(got, want) {
		t.Fatalf("domains = %q, want %q", got, want)
	}
	// a second call reuses the session
	if _, err := c.ListDomains(context.Background(), technitium.Allowed); err != nil {
		t.Fatal(err)
	}
	if n := len(s.reqs("/api/user/login")); n != 1 {
		t.Fatalf("%d logins, want 1", n)
	}
	for _, r := range s.reqs("/api/allowed/export") {
		if r.Auth != "Bearer tok1" || r.Method != http.MethodPost {
			t.Fatalf("request %+v", r)
		}
	}
}

func TestLoginRefused(t *testing.T) {
	s := newServer(t)
	passwordLogin(s, "tok1")
	c := technitium.NewHTTPClient(s.URL, technitium.Credentials{Username: "admin", Password: "wrong"}, nil)
	_, err := c.GetSettings(context.Background())
	if !errors.Is(err, technitium.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestRelogsInOnInvalidToken(t *testing.T) {
	s := newServer(t)
	passwordLogin(s, "old", "new")
	s.handle("/api/allowed/add", func(w http.ResponseWriter, form url.Values) {
		s.mu.Lock()
		last := s.requests[len(s.requests)-1]
		s.mu.Unlock()
		if last.Auth != "Bearer new" {
			_, _ = io.WriteString(w, `{"status":"invalid-token","errorMessage":"Invalid token or session expired."}`)
			return
		}
		if form.Get("domain") != "example.com" {
			t.Errorf("form = %v", form)
		}
		_, _ = io.WriteString(w, okEmpty)
	})
	c := technitium.NewHTTPClient(s.URL, admin, nil)
	if err := c.AddDomain(context.Background(), technitium.Allowed, "example.com"); err != nil {
		t.Fatal(err)
	}
	if n := len(s.reqs("/api/user/login")); n != 2 {
		t.Fatalf("%d logins, want 2", n)
	}
	if n := len(s.reqs("/api/allowed/add")); n != 2 {
		t.Fatalf("%d adds, want 2 (one retry)", n)
	}
}

func TestInvalidTokenTwiceIsAuthError(t *testing.T) {
	s := newServer(t)
	passwordLogin(s, "tok")
	s.handle("/api/settings/get", reply(`{"status":"invalid-token","errorMessage":"nope"}`))
	c := technitium.NewHTTPClient(s.URL, admin, nil)
	if _, err := c.GetSettings(context.Background()); !errors.Is(err, technitium.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestAPIToken(t *testing.T) {
	s := newServer(t)
	s.handle("/api/blocked/delete", reply(okEmpty))
	s.handle("/api/settings/get", reply(`{"status":"invalid-token"}`))
	c := technitium.NewHTTPClient(s.URL, technitium.Credentials{Token: "apitok", Username: "ignored"}, nil)
	if err := c.DeleteDomain(context.Background(), technitium.Blocked, "bad.org"); err != nil {
		t.Fatal(err)
	}
	if r := s.reqs("/api/blocked/delete")[0]; r.Auth != "Bearer apitok" || r.Form.Get("domain") != "bad.org" {
		t.Fatalf("request %+v", r)
	}
	// a refused API token is an auth error and never triggers a login
	if _, err := c.GetSettings(context.Background()); !errors.Is(err, technitium.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.reqs("/api/user/login"))+len(s.reqs("/api/user/logout")) != 0 {
		t.Fatal("API token must not log in or out")
	}
}

func TestErrorStatusBecomesAPIError(t *testing.T) {
	s := newServer(t)
	passwordLogin(s, "tok")
	s.handle("/api/allowed/add", reply(`{"status":"error","errorMessage":"Invalid domain name [x!]","stackTrace":"..."}`))
	c := technitium.NewHTTPClient(s.URL, admin, nil)
	err := c.AddDomain(context.Background(), technitium.Allowed, "x!")
	var apiErr *technitium.APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "Invalid domain name") {
		t.Fatalf("err = %v", err)
	}
}

func TestExportErrorIsNotADomainList(t *testing.T) {
	s := newServer(t)
	passwordLogin(s, "tok")
	s.handle("/api/allowed/export", reply(`{"status":"error","errorMessage":"Access was denied."}`))
	c := technitium.NewHTTPClient(s.URL, admin, nil)
	if got, err := c.ListDomains(context.Background(), technitium.Allowed); err == nil {
		t.Fatalf("got %q, want an error", got)
	}
}

func TestSettings(t *testing.T) {
	s := newServer(t)
	passwordLogin(s, "tok")
	s.handle("/api/settings/get", reply(`{"response":{"version":"15.6.2","blockListUrls":["https://a/x","https://b/y"]},"status":"ok"}`))
	s.handle("/api/settings/set", reply(`{"response":{"version":"15.6.2"},"status":"ok"}`))
	c := technitium.NewHTTPClient(s.URL, admin, nil)
	ctx := context.Background()

	got, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "15.6.2" || !slices.Equal(got.BlockListURLs, []string{"https://a/x", "https://b/y"}) {
		t.Fatalf("settings = %+v", got)
	}

	if err := c.SetBlockListURLs(ctx, []string{"https://a/x", "https://c/z"}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetBlockListURLs(ctx, nil); err != nil {
		t.Fatal(err)
	}
	sets := s.reqs("/api/settings/set")
	if v := sets[0].Form.Get("blockListUrls"); v != "https://a/x,https://c/z" {
		t.Fatalf("blockListUrls = %q", v)
	}
	if v := sets[1].Form.Get("blockListUrls"); v != "false" {
		t.Fatalf("clearing sent %q, want false", v)
	}
	if err := c.SetBlockListURLs(ctx, []string{"https://a/x,y"}); err == nil {
		t.Fatal("a comma in a URL must be refused")
	}
	if len(s.reqs("/api/settings/set")) != 2 {
		t.Fatal("refused URL list was sent")
	}
}

func TestSettingsWithoutBlockLists(t *testing.T) {
	s := newServer(t)
	passwordLogin(s, "tok")
	s.handle("/api/settings/get", reply(`{"response":{"version":"15.6.2","blockListUrls":null},"status":"ok"}`))
	c := technitium.NewHTTPClient(s.URL, admin, nil)
	got, err := c.GetSettings(context.Background())
	if err != nil || len(got.BlockListURLs) != 0 {
		t.Fatalf("settings = %+v, err = %v", got, err)
	}
}

func TestAppConfig(t *testing.T) {
	s := newServer(t)
	passwordLogin(s, "tok")
	s.handle("/api/apps/config/get", func(w http.ResponseWriter, form url.Values) {
		switch form.Get("name") {
		case "Query Logs (Sqlite)":
			_, _ = io.WriteString(w, `{"response":{"config":"{\r\n  \"maxLogDays\": 0\r\n}"},"status":"ok"}`)
		case "Empty":
			_, _ = io.WriteString(w, `{"response":{"config":null},"status":"ok"}`)
		default:
			_, _ = io.WriteString(w, `{"status":"error","errorMessage":"DNS application was not found: `+form.Get("name")+`"}`)
		}
	})
	s.handle("/api/apps/config/set", reply(okEmpty))
	c := technitium.NewHTTPClient(s.URL, admin, nil)
	ctx := context.Background()

	cfg, err := c.GetAppConfig(ctx, "Query Logs (Sqlite)")
	if err != nil || !strings.Contains(cfg, `"maxLogDays": 0`) {
		t.Fatalf("config = %q, err = %v", cfg, err)
	}
	if cfg, err = c.GetAppConfig(ctx, "Empty"); err != nil || cfg != "" {
		t.Fatalf("config = %q, err = %v", cfg, err)
	}
	if _, err = c.GetAppConfig(ctx, "Nope"); !errors.Is(err, technitium.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := c.SetAppConfig(ctx, "Query Logs (Sqlite)", `{"maxLogDays":7}`); err != nil {
		t.Fatal(err)
	}
	r := s.reqs("/api/apps/config/set")[0]
	if r.Form.Get("name") != "Query Logs (Sqlite)" || r.Form.Get("config") != `{"maxLogDays":7}` {
		t.Fatalf("form = %v", r.Form)
	}
}

func TestCloseLogsOut(t *testing.T) {
	s := newServer(t)
	passwordLogin(s, "tok")
	s.handle("/api/settings/get", reply(`{"response":{"version":"1"},"status":"ok"}`))
	s.handle("/api/user/logout", reply(`{"status":"ok"}`))
	c := technitium.NewHTTPClient(s.URL, admin, nil)
	ctx := context.Background()
	if err := c.Close(ctx); err != nil { // never logged in: nothing to do
		t.Fatal(err)
	}
	if _, err := c.GetSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	out := s.reqs("/api/user/logout")
	if len(out) != 1 || out[0].Auth != "Bearer tok" {
		t.Fatalf("logout requests = %+v", out)
	}
	if err := c.Close(ctx); err != nil || len(s.reqs("/api/user/logout")) != 1 {
		t.Fatal("second Close must not log out again")
	}
}

func TestHTTPFailure(t *testing.T) {
	s := newServer(t)
	passwordLogin(s, "tok")
	s.handle("/api/settings/get", func(w http.ResponseWriter, _ url.Values) { http.Error(w, "boom", http.StatusBadGateway) })
	c := technitium.NewHTTPClient(s.URL, admin, nil)
	if _, err := c.GetSettings(context.Background()); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
}
