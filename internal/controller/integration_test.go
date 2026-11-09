//go:build integration

package controller

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	techv1alpha1 "github.com/jigfox/technitium-operator/api/v1alpha1"
	"github.com/jigfox/technitium-operator/internal/technitium"
	"github.com/jigfox/technitium-operator/internal/technitium/technitiumtest"
)

// recordingProvider wraps a ClientProvider and records every mutation sent.
type recordingProvider struct {
	inner ClientProvider
	mu    sync.Mutex
	calls []string
}

func (p *recordingProvider) ClientFor(ctx context.Context, s *techv1alpha1.TechnitiumServer) (technitium.Client, error) {
	c, err := p.inner.ClientFor(ctx, s)
	if err != nil {
		return nil, err
	}
	return &recordingClient{Client: c, p: p}, nil
}

func (p *recordingProvider) Forget(ctx context.Context, key types.NamespacedName) {
	p.inner.Forget(ctx, key)
}

func (p *recordingProvider) take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.calls
	p.calls = nil
	return out
}

type recordingClient struct {
	technitium.Client
	p *recordingProvider
}

func (c *recordingClient) record(s string) {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	c.p.calls = append(c.p.calls, s)
}

func (c *recordingClient) SetBlockListURLs(ctx context.Context, urls []string) error {
	c.record("set blocklisturls")
	return c.Client.SetBlockListURLs(ctx, urls)
}

func (c *recordingClient) AddDomain(ctx context.Context, l technitium.List, d string) error {
	c.record("add " + string(l) + " " + d)
	return c.Client.AddDomain(ctx, l, d)
}

func (c *recordingClient) DeleteDomain(ctx context.Context, l technitium.List, d string) error {
	c.record("delete " + string(l) + " " + d)
	return c.Client.DeleteDomain(ctx, l, d)
}

func (c *recordingClient) SetAppConfig(ctx context.Context, a, cfg string) error {
	c.record("set appconfig " + a)
	return c.Client.SetAppConfig(ctx, a, cfg)
}

func (e *env) realServer(ctx context.Context, t *testing.T) (*technitiumtest.Server, technitium.Client, *recordingProvider) {
	t.Helper()
	real := technitiumtest.Start(ctx, t)
	token := real.CreateAPIToken(t, "controller-test")
	if err := e.c.Create(ctx, secret("creds", map[string]string{
		"username": real.Username, "password": real.Password, "token": token, "wrong": "not-the-password"})); err != nil {
		t.Fatal(err)
	}
	prov := &recordingProvider{inner: &SecretClientProvider{Reader: e.c}}
	e.srv.Provider, e.allw.Provider, e.blk.Provider = prov, prov, prov
	direct := technitium.NewHTTPClient(real.URL, technitium.Credentials{Username: real.Username, Password: real.Password}, nil)
	t.Cleanup(func() { _ = direct.Close(ctx) })
	return real, direct, prov
}

func setURL(url string) func(*techv1alpha1.TechnitiumServer) {
	return func(s *techv1alpha1.TechnitiumServer) { s.Spec.URL = url }
}

func list(t *testing.T, c technitium.Client, l technitium.List) []string {
	t.Helper()
	got, err := c.ListDomains(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	return got
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) && len(got)+len(want) > 0 {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

// TestReconcileAgainstRealTechnitium runs the reconcilers (with a fake
// Kubernetes client) against a real Technitium: adoption, convergence,
// idempotence, deletion, pruning, settings and apps.
// nolint:gocyclo // one long scenario against a single container
func TestReconcileAgainstRealTechnitium(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, false) // the objects are created per subtest
	real, direct, prov := e.realServer(ctx, t)
	create := func(objs ...client.Object) {
		t.Helper()
		for _, o := range objs {
			if err := e.c.Create(ctx, o); err != nil {
				t.Fatal(err)
			}
		}
	}
	create(srv("primary", setURL(real.URL)))

	t.Run("adopt, add, converge and delete", func(t *testing.T) {
		for _, d := range []string{"adopted.example.com", "untouched.example.com"} {
			if err := direct.AddDomain(ctx, technitium.Allowed, d); err != nil {
				t.Fatal(err)
			}
		}
		create(
			allowedDomain("adopted", "Adopted.Example.com"),
			allowedDomain("fresh", "fresh.example.com."),
			allowedDomain("kept", "kept.example.com", orphan),
			blockedDomain("ads", "ads.example.org"),
		)
		for _, n := range []string{"adopted", "fresh", "kept"} {
			e.reconcileAllowed(n)
		}
		e.reconcileBlocked("ads")

		equal(t, "allowed", list(t, direct, technitium.Allowed),
			[]string{"adopted.example.com", "fresh.example.com", "kept.example.com", "untouched.example.com"})
		equal(t, "blocked", list(t, direct, technitium.Blocked), []string{"ads.example.org"})
		if d := e.allowed("adopted"); !d.Status.Adopted || d.Status.Domain != "adopted.example.com" {
			t.Fatalf("adopted: %+v", d.Status)
		}
		if d := e.allowed("fresh"); d.Status.Adopted || d.Status.Domain != "fresh.example.com" {
			t.Fatalf("fresh: %+v", d.Status)
		}
		for _, n := range []string{"adopted", "fresh", "kept"} {
			if c := ready(t, e.allowed(n).Status.Conditions); c.Status != metav1.ConditionTrue {
				t.Fatalf("%s: %+v", n, c)
			}
		}
		prov.take()

		// a second pass changes nothing
		for _, n := range []string{"adopted", "fresh", "kept"} {
			e.reconcileAllowed(n)
		}
		e.reconcileBlocked("ads")
		equal(t, "mutations of the second pass", prov.take(), nil)

		// drift made in the UI is corrected
		if err := direct.DeleteDomain(ctx, technitium.Allowed, "fresh.example.com"); err != nil {
			t.Fatal(err)
		}
		e.reconcileAllowed("fresh")
		equal(t, "mutations after drift", prov.take(), []string{"add allowed fresh.example.com"})

		// Delete removes (also an adopted entry), Orphan keeps, the rest stays
		for _, n := range []string{"adopted", "kept"} {
			if err := e.c.Delete(ctx, e.allowed(n)); err != nil {
				t.Fatal(err)
			}
			e.reconcileAllowed(n)
		}
		equal(t, "allowed after deletes", list(t, direct, technitium.Allowed),
			[]string{"fresh.example.com", "kept.example.com", "untouched.example.com"})
	})

	t.Run("prune", func(t *testing.T) {
		// state from before: fresh (managed), kept + untouched (unmanaged now)
		if err := direct.AddDomain(ctx, technitium.Blocked, "stray.example.org"); err != nil {
			t.Fatal(err)
		}
		s := e.server()
		s.Spec.PruneUnmanaged = techv1alpha1.PruneUnmanaged{AllowedDomains: true, BlockedDomains: true}
		if err := e.c.Update(ctx, s); err != nil {
			t.Fatal(err)
		}

		// never before the cache is synced
		e.srv.Cache = unsynced{}
		e.mustReconcileServer()
		equal(t, "allowed with an unsynced cache", list(t, direct, technitium.Allowed),
			[]string{"fresh.example.com", "kept.example.com", "untouched.example.com"})
		if c := ready(t, e.server().Status.Conditions); c.Status != metav1.ConditionFalse ||
			!strings.Contains(c.Message, "cache is not synced") {
			t.Fatalf("Ready = %+v", c)
		}

		e.srv.Cache = nil
		e.mustReconcileServer()
		equal(t, "allowed after pruning", list(t, direct, technitium.Allowed), []string{"fresh.example.com"})
		equal(t, "blocked after pruning", list(t, direct, technitium.Blocked), []string{"ads.example.org"})
		prov.take()
		e.mustReconcileServer()
		equal(t, "mutations of a second prune", prov.take(), nil)
	})

	t.Run("block list URLs are written once", func(t *testing.T) {
		urls := []string{"https://lists.invalid/a.txt", "https://lists.invalid/b.txt"}
		s := e.server()
		s.Spec.Settings = &techv1alpha1.ServerSettings{BlockListURLs: &urls}
		if err := e.c.Update(ctx, s); err != nil {
			t.Fatal(err)
		}
		prov.take()
		e.mustReconcileServer()
		equal(t, "first write", prov.take(), []string{"set blocklisturls"})
		e.mustReconcileServer()
		equal(t, "second write", prov.take(), nil)
		if got, _ := direct.GetSettings(ctx); !slices.Equal(got.BlockListURLs, urls) {
			t.Fatalf("live urls = %q", got.BlockListURLs)
		}

		cleared := []string{}
		s = e.server()
		s.Spec.Settings.BlockListURLs = &cleared
		if err := e.c.Update(ctx, s); err != nil {
			t.Fatal(err)
		}
		e.mustReconcileServer()
		equal(t, "clearing", prov.take(), []string{"set blocklisturls"})
		if got, _ := direct.GetSettings(ctx); len(got.BlockListURLs) != 0 {
			t.Fatalf("live urls = %q", got.BlockListURLs)
		}
		e.mustReconcileServer()
		equal(t, "after clearing", prov.take(), nil)
	})

	t.Run("app config merges over the live one", func(t *testing.T) {
		const app = "Query Logs (Sqlite)"
		real.InstallStoreApp(t, app)
		before, err := direct.GetAppConfig(ctx, app)
		if err != nil {
			t.Fatal(err)
		}
		liveBefore, _ := decodeObject(before)

		s := e.server()
		s.Spec.Apps = []techv1alpha1.AppConfig{
			{Name: app, Config: jsonOf(`{"enableLogging": true, "maxLogDays": 14}`)},
			{Name: "Not Installed", Config: jsonOf(`{"a": 1}`)},
		}
		if err := e.c.Update(ctx, s); err != nil {
			t.Fatal(err)
		}
		prov.take()
		e.mustReconcileServer()
		equal(t, "first write", prov.take(), []string{"set appconfig " + app})
		after, _ := direct.GetAppConfig(ctx, app)
		liveAfter, err := decodeObject(after)
		if err != nil {
			t.Fatal(err)
		}
		if liveAfter["maxLogDays"].(interface{ String() string }).String() != "14" || liveAfter["enableLogging"] != true {
			t.Fatalf("managed keys not applied: %s", after)
		}
		for k, v := range liveBefore {
			if k != "maxLogDays" && k != "enableLogging" && !jsonEqual(v, liveAfter[k]) {
				t.Fatalf("unmanaged key %q changed from %v to %v", k, v, liveAfter[k])
			}
		}
		if c := ready(t, e.server().Status.Conditions); c.Reason != techv1alpha1.ReasonAppError ||
			!strings.Contains(c.Message, `"Not Installed"`) {
			t.Fatalf("Ready = %+v", c)
		}

		// nothing to write the second time, a change in the UI is undone
		e.mustReconcileServer()
		equal(t, "second write", prov.take(), nil)
		if err := direct.SetAppConfig(ctx, app, strings.Replace(after, "14", "99", 1)); err != nil {
			t.Fatal(err)
		}
		e.mustReconcileServer()
		equal(t, "drift", prov.take(), []string{"set appconfig " + app})
	})

	t.Run("credentials from the Secret", func(t *testing.T) {
		ref := func(r techv1alpha1.CredentialsSecretRef) {
			s := e.server()
			s.Spec.CredentialsSecretRef = r
			if err := e.c.Update(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		for name, r := range map[string]techv1alpha1.CredentialsSecretRef{
			"literal username":   {Name: "creds", Username: "admin"},
			"API token":          {Name: "creds", TokenKey: "token"},
			"explicit key names": {Name: "creds", UsernameKey: "username", PasswordKey: "password"},
		} {
			ref(r)
			e.mustReconcileServer()
			if c := ready(t, e.server().Status.Conditions); c.Reason == techv1alpha1.ReasonCredentials ||
				c.Reason == techv1alpha1.ReasonTechnitium {
				t.Fatalf("%s: Ready = %+v", name, c)
			}
		}
		ref(techv1alpha1.CredentialsSecretRef{Name: "creds", PasswordKey: "wrong"})
		e.mustReconcileServer()
		if c := ready(t, e.server().Status.Conditions); c.Reason != techv1alpha1.ReasonCredentials ||
			c.Status != metav1.ConditionFalse {
			t.Fatalf("wrong password: Ready = %+v", c)
		}
		ref(techv1alpha1.CredentialsSecretRef{Name: "missing"})
		e.mustReconcileServer()
		if c := ready(t, e.server().Status.Conditions); c.Reason != techv1alpha1.ReasonCredentials ||
			!strings.Contains(c.Message, `"missing" not found`) {
			t.Fatalf("missing secret: Ready = %+v", c)
		}
	})
}
