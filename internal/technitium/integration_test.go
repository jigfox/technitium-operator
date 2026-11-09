//go:build integration

package technitium_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jigfox/technitium-operator/internal/technitium"
	"github.com/jigfox/technitium-operator/internal/technitium/technitiumtest"
)

// nolint:gocyclo // one long scenario against a single container
func TestAgainstRealTechnitium(t *testing.T) {
	ctx := context.Background()
	srv := technitiumtest.Start(ctx, t)
	c := technitium.NewHTTPClient(srv.URL, technitium.Credentials{Username: srv.Username, Password: srv.Password}, nil)
	t.Cleanup(func() { _ = c.Close(ctx) })

	settings, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(settings.Version, "15.") || len(settings.BlockListURLs) != 0 {
		t.Fatalf("settings = %+v", settings)
	}

	for _, list := range []technitium.List{technitium.Allowed, technitium.Blocked} {
		t.Run(string(list), func(t *testing.T) {
			if got, err := c.ListDomains(ctx, list); err != nil || len(got) != 0 {
				t.Fatalf("fresh list = %q, %v", got, err)
			}
			// adding twice is fine, and the case is folded
			for range 2 {
				if err := c.AddDomain(ctx, list, "Example.COM"); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.AddDomain(ctx, list, "second.example.org"); err != nil {
				t.Fatal(err)
			}
			got, err := c.ListDomains(ctx, list)
			if err != nil || !slices.Equal(got, []string{"example.com", "second.example.org"}) {
				t.Fatalf("list = %q, %v", got, err)
			}
			// a trailing dot is not accepted by Technitium, which is why the
			// controllers normalise
			var apiErr *technitium.APIError
			if err := c.AddDomain(ctx, list, "example.net."); !errors.As(err, &apiErr) {
				t.Fatalf("trailing dot: err = %v, want an APIError", err)
			}
			if err := c.DeleteDomain(ctx, list, "example.com"); err != nil {
				t.Fatal(err)
			}
			if err := c.DeleteDomain(ctx, list, "never-there.example.com"); err != nil {
				t.Fatalf("deleting a missing entry: %v", err)
			}
			if got, _ = c.ListDomains(ctx, list); !slices.Equal(got, []string{"second.example.org"}) {
				t.Fatalf("list = %q", got)
			}
		})
	}
	// the lists are separate
	if got, _ := c.ListDomains(ctx, technitium.Allowed); !slices.Equal(got, []string{"second.example.org"}) {
		t.Fatal(got)
	}

	t.Run("block list URLs", func(t *testing.T) {
		urls := []string{"https://lists.invalid/a.txt", "https://lists.invalid/b.txt"}
		if err := c.SetBlockListURLs(ctx, urls); err != nil {
			t.Fatal(err)
		}
		if s, _ := c.GetSettings(ctx); !slices.Equal(s.BlockListURLs, urls) {
			t.Fatalf("urls = %q", s.BlockListURLs)
		}
		if err := c.SetBlockListURLs(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if s, _ := c.GetSettings(ctx); len(s.BlockListURLs) != 0 {
			t.Fatalf("urls after clearing = %q", s.BlockListURLs)
		}
	})

	t.Run("unknown app", func(t *testing.T) {
		if _, err := c.GetAppConfig(ctx, "No Such App"); !errors.Is(err, technitium.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if err := c.SetAppConfig(ctx, "No Such App", "{}"); !errors.Is(err, technitium.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("app config", func(t *testing.T) {
		const app = "Query Logs (Sqlite)"
		srv.InstallStoreApp(t, app)
		live, err := c.GetAppConfig(ctx, app)
		if err != nil || !strings.Contains(live, "maxLogDays") {
			t.Fatalf("config = %q, %v", live, err)
		}
		if err := c.SetAppConfig(ctx, app, `{"enableLogging": true, "maxLogDays": 14}`); err != nil {
			t.Fatal(err)
		}
		if live, _ = c.GetAppConfig(ctx, app); !strings.Contains(live, "14") {
			t.Fatalf("config = %q", live)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		bad := technitium.NewHTTPClient(srv.URL, technitium.Credentials{Username: srv.Username, Password: "nope"}, nil)
		if _, err := bad.GetSettings(ctx); !errors.Is(err, technitium.ErrAuth) {
			t.Fatalf("err = %v, want ErrAuth", err)
		}
	})

	t.Run("API token", func(t *testing.T) {
		tok := srv.CreateAPIToken(t, "operator-test")
		tc := technitium.NewHTTPClient(srv.URL, technitium.Credentials{Token: tok}, nil)
		if s, err := tc.GetSettings(ctx); err != nil || s.Version == "" {
			t.Fatalf("settings = %+v, %v", s, err)
		}
		bad := technitium.NewHTTPClient(srv.URL, technitium.Credentials{Token: "00" + tok[2:]}, nil)
		if _, err := bad.GetSettings(ctx); !errors.Is(err, technitium.ErrAuth) {
			t.Fatalf("err = %v, want ErrAuth", err)
		}
	})

	t.Run("session ends on Close", func(t *testing.T) {
		cc := technitium.NewHTTPClient(srv.URL, technitium.Credentials{Username: srv.Username, Password: srv.Password}, nil)
		if _, err := cc.GetSettings(ctx); err != nil {
			t.Fatal(err)
		}
		if err := cc.Close(ctx); err != nil {
			t.Fatal(err)
		}
		// the next call logs in again
		if _, err := cc.GetSettings(ctx); err != nil {
			t.Fatal(err)
		}
		_ = cc.Close(ctx)
	})
}
