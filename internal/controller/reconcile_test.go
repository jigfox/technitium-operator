package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	techv1alpha1 "github.com/jigfox/technitium-operator/api/v1alpha1"
	"github.com/jigfox/technitium-operator/internal/technitium"
	faketech "github.com/jigfox/technitium-operator/internal/technitium/fake"
)

const ns = "dns"

// fakeProvider hands out one fake Technitium per server name.
type fakeProvider struct {
	servers   map[string]technitium.Client
	credErr   error
	forgotten []string
}

func (p *fakeProvider) ClientFor(_ context.Context, s *techv1alpha1.TechnitiumServer) (technitium.Client, error) {
	if p.credErr != nil {
		return nil, &CredentialsError{Err: p.credErr}
	}
	return p.servers[s.Name], nil
}

func (p *fakeProvider) Forget(_ context.Context, key types.NamespacedName) {
	p.forgotten = append(p.forgotten, key.Name)
}

// unsynced is a cache that never syncs.
type unsynced struct{}

func (unsynced) WaitForCacheSync(context.Context) bool { return false }

type env struct {
	t    *testing.T
	c    client.Client
	tech *faketech.Client
	prov *fakeProvider
	rec  *events.FakeRecorder
	srv  *ServerReconciler
	allw *DomainReconciler
	blk  *DomainReconciler
	last ctrl.Result
}

func newEnv(t *testing.T, dryRun bool, objs ...client.Object) *env {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := techv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&techv1alpha1.TechnitiumServer{}, &techv1alpha1.TechnitiumAllowedDomain{},
			&techv1alpha1.TechnitiumBlockedDomain{}).
		WithObjects(objs...).Build()
	tech := faketech.New()
	prov := &fakeProvider{servers: map[string]technitium.Client{"primary": tech}}
	rec := events.NewFakeRecorder(200)
	return &env{
		t: t, c: c, tech: tech, prov: prov, rec: rec,
		srv:  &ServerReconciler{Client: c, Scheme: scheme, Provider: prov, Recorder: rec, DryRun: dryRun},
		allw: &DomainReconciler{Client: c, Scheme: scheme, Provider: prov, Recorder: rec, DryRun: dryRun, List: technitium.Allowed},
		blk:  &DomainReconciler{Client: c, Scheme: scheme, Provider: prov, Recorder: rec, DryRun: dryRun, List: technitium.Blocked},
	}
}

func req(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

func (e *env) reconcileServer(name string) error {
	e.t.Helper()
	res, err := e.srv.Reconcile(context.Background(), req(name))
	e.last = res
	return err
}

func (e *env) mustReconcileServer() {
	e.t.Helper()
	if err := e.reconcileServer("primary"); err != nil {
		e.t.Fatalf("reconcile server: %v", err)
	}
}

func (e *env) reconcileAllowed(name string) {
	e.t.Helper()
	res, err := e.allw.Reconcile(context.Background(), req(name))
	if err != nil {
		e.t.Fatalf("reconcile allowed %s: %v", name, err)
	}
	e.last = res
}

func (e *env) reconcileBlocked(name string) {
	e.t.Helper()
	res, err := e.blk.Reconcile(context.Background(), req(name))
	if err != nil {
		e.t.Fatalf("reconcile blocked %s: %v", name, err)
	}
	e.last = res
}

func (e *env) server() *techv1alpha1.TechnitiumServer {
	e.t.Helper()
	var s techv1alpha1.TechnitiumServer
	if err := e.c.Get(context.Background(), req("primary").NamespacedName, &s); err != nil {
		e.t.Fatal(err)
	}
	return &s
}

func (e *env) allowed(name string) *techv1alpha1.TechnitiumAllowedDomain {
	e.t.Helper()
	var d techv1alpha1.TechnitiumAllowedDomain
	if err := e.c.Get(context.Background(), req(name).NamespacedName, &d); err != nil {
		e.t.Fatal(err)
	}
	return &d
}

func (e *env) events() []string {
	var out []string
	for {
		select {
		case ev := <-e.rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func srv(name string, mod ...func(*techv1alpha1.TechnitiumServer)) *techv1alpha1.TechnitiumServer {
	s := &techv1alpha1.TechnitiumServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1},
		Spec: techv1alpha1.TechnitiumServerSpec{
			URL:                  "http://technitium.invalid:5380",
			CredentialsSecretRef: techv1alpha1.CredentialsSecretRef{Name: "creds"},
		},
	}
	for _, m := range mod {
		m(s)
	}
	return s
}

func allowedDomain(name, domain string, mod ...func(*techv1alpha1.DomainSpec)) *techv1alpha1.TechnitiumAllowedDomain {
	d := &techv1alpha1.TechnitiumAllowedDomain{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1},
		Spec:       techv1alpha1.DomainSpec{ServerRef: techv1alpha1.ServerReference{Name: "primary"}, Domain: domain},
	}
	for _, m := range mod {
		m(&d.Spec)
	}
	return d
}

func blockedDomain(name, domain string, mod ...func(*techv1alpha1.DomainSpec)) *techv1alpha1.TechnitiumBlockedDomain {
	d := &techv1alpha1.TechnitiumBlockedDomain{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1},
		Spec:       techv1alpha1.DomainSpec{ServerRef: techv1alpha1.ServerReference{Name: "primary"}, Domain: domain},
	}
	for _, m := range mod {
		m(&d.Spec)
	}
	return d
}

func orphan(s *techv1alpha1.DomainSpec) { s.DeletionPolicy = techv1alpha1.DeletionPolicyOrphan }

func ready(t *testing.T, conds []metav1.Condition) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(conds, techv1alpha1.ConditionReady)
	if c == nil {
		t.Fatal("no Ready condition")
	}
	return c
}

func expectCalls(t *testing.T, l *faketech.Client, want ...string) {
	t.Helper()
	got := l.Calls()
	if !slices.Equal(got, want) && (len(got) != 0 || len(want) != 0) {
		t.Fatalf("technitium calls = %q, want %q", got, want)
	}
}

func jsonOf(s string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(s)} }

// ---- domain resources ----

func TestDomainAddedNormalisedAndIdempotent(t *testing.T) {
	e := newEnv(t, false, srv("primary"), allowedDomain("ex", "Example.COM."))

	e.reconcileAllowed("ex")
	expectCalls(t, e.tech, "add allowed example.com")
	d := e.allowed("ex")
	if d.Status.Domain != "example.com" || d.Status.Adopted || !slices.Contains(d.Finalizers, Finalizer) {
		t.Fatalf("status = %+v, finalizers %v", d.Status, d.Finalizers)
	}
	if c := ready(t, d.Status.Conditions); c.Status != metav1.ConditionTrue || c.Reason != techv1alpha1.ReasonReconciled {
		t.Fatalf("Ready = %+v", c)
	}
	if d.Status.ObservedGeneration != 1 || e.last.RequeueAfter != DefaultInterval {
		t.Fatalf("observedGeneration %d, requeue %v", d.Status.ObservedGeneration, e.last.RequeueAfter)
	}

	e.tech.ResetCalls()
	e.reconcileAllowed("ex")
	expectCalls(t, e.tech)
}

func TestDomainReAddedAfterDriftButStaysNotAdopted(t *testing.T) {
	e := newEnv(t, false, srv("primary"), allowedDomain("ex", "example.com"))
	e.reconcileAllowed("ex")
	e.tech.ResetCalls()
	if err := e.tech.DeleteDomain(context.Background(), technitium.Allowed, "example.com"); err != nil {
		t.Fatal(err)
	}
	e.tech.ResetCalls()
	e.reconcileAllowed("ex")
	expectCalls(t, e.tech, "add allowed example.com")
	if e.allowed("ex").Status.Adopted {
		t.Fatal("a re-added entry is not adopted")
	}
}

func TestDomainAdoptsExistingEntry(t *testing.T) {
	e := newEnv(t, false, srv("primary"), allowedDomain("ex", "example.com"))
	e.tech.Seed(technitium.Allowed, "example.com")

	e.reconcileAllowed("ex")
	expectCalls(t, e.tech)
	d := e.allowed("ex")
	if !d.Status.Adopted || d.Status.Domain != "example.com" {
		t.Fatalf("status = %+v", d.Status)
	}
	if !slices.ContainsFunc(e.events(), func(s string) bool { return strings.Contains(s, "Adopted") }) {
		t.Fatal("no Adopted event")
	}
}

func TestDomainListsAreSeparate(t *testing.T) {
	e := newEnv(t, false, srv("primary"), blockedDomain("bad", "ads.example.org"))
	e.tech.Seed(technitium.Allowed, "ads.example.org") // same name in the other list is no adoption
	e.reconcileBlocked("bad")
	expectCalls(t, e.tech, "add blocked ads.example.org")
	if e.c.Scheme() == nil {
		t.Fatal("unreachable")
	}
}

func TestDomainDeletionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mod        []func(*techv1alpha1.DomainSpec)
		adopted    bool
		wantCalls  []string
		wantRemain []string
	}{
		{"delete", nil, false, []string{"delete allowed example.com"}, nil},
		{"delete also removes an adopted entry", nil, true, []string{"delete allowed example.com"}, nil},
		{"orphan keeps it", []func(*techv1alpha1.DomainSpec){orphan}, false, nil, []string{"example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, false, srv("primary"), allowedDomain("ex", "example.com", tc.mod...))
			if tc.adopted {
				e.tech.Seed(technitium.Allowed, "example.com")
			}
			e.reconcileAllowed("ex")
			e.tech.ResetCalls()
			if err := e.c.Delete(context.Background(), e.allowed("ex")); err != nil {
				t.Fatal(err)
			}
			e.reconcileAllowed("ex")
			expectCalls(t, e.tech, tc.wantCalls...)
			if got := e.tech.Domains(technitium.Allowed); !slices.Equal(got, tc.wantRemain) && len(got)+len(tc.wantRemain) > 0 {
				t.Fatalf("entries = %q, want %q", got, tc.wantRemain)
			}
			var d techv1alpha1.TechnitiumAllowedDomain
			if err := e.c.Get(context.Background(), req("ex").NamespacedName, &d); err == nil {
				t.Fatalf("resource still there, finalizers %v", d.Finalizers)
			}
		})
	}
}

func TestDomainNeverReachedTechnitiumDeletesNothing(t *testing.T) {
	// the server is unreachable, so status.domain stays empty; an entry that
	// someone else created meanwhile must not be deleted with the resource
	e := newEnv(t, false, srv("primary"), allowedDomain("ex", "example.com"))
	e.tech.Err = errors.New("down")
	if _, err := e.allw.Reconcile(context.Background(), req("ex")); err == nil {
		t.Fatal("want an error")
	}
	e.tech.Err = nil
	e.tech.Seed(technitium.Allowed, "example.com")
	if err := e.c.Delete(context.Background(), e.allowed("ex")); err != nil {
		t.Fatal(err)
	}
	e.reconcileAllowed("ex")
	expectCalls(t, e.tech)
	if got := e.tech.Domains(technitium.Allowed); !slices.Equal(got, []string{"example.com"}) {
		t.Fatalf("entries = %q", got)
	}
}

func TestDomainServerMissing(t *testing.T) {
	e := newEnv(t, false, allowedDomain("ex", "example.com"))
	e.reconcileAllowed("ex")
	c := ready(t, e.allowed("ex").Status.Conditions)
	if c.Status != metav1.ConditionFalse || c.Reason != techv1alpha1.ReasonServerNotFound {
		t.Fatalf("Ready = %+v", c)
	}
	if e.last.RequeueAfter != retryInterval {
		t.Fatalf("requeue = %v", e.last.RequeueAfter)
	}
	// deleting it must not hang on the missing server
	if err := e.c.Delete(context.Background(), e.allowed("ex")); err != nil {
		t.Fatal(err)
	}
	e.reconcileAllowed("ex")
	var d techv1alpha1.TechnitiumAllowedDomain
	if err := e.c.Get(context.Background(), req("ex").NamespacedName, &d); err == nil {
		t.Fatal("finalizer blocks the deletion")
	}
}

func TestDomainDeletedWithServerGoneLeavesEntry(t *testing.T) {
	e := newEnv(t, false, srv("primary"), allowedDomain("ex", "example.com"))
	e.reconcileAllowed("ex")
	if err := e.c.Delete(context.Background(), e.server()); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), e.allowed("ex")); err != nil {
		t.Fatal(err)
	}
	e.events()
	e.reconcileAllowed("ex")
	if got := e.tech.Domains(technitium.Allowed); !slices.Equal(got, []string{"example.com"}) {
		t.Fatalf("entries = %q", got)
	}
	if !slices.ContainsFunc(e.events(), func(s string) bool { return strings.Contains(s, "ServerNotFound") }) {
		t.Fatal("no Warning event")
	}
}

func TestDomainErrors(t *testing.T) {
	e := newEnv(t, false, srv("primary"), allowedDomain("ex", "example.com"))

	e.prov.credErr = errors.New(`secret "creds" not found`)
	e.reconcileAllowed("ex") // no error: only a change of the Secret helps
	c := ready(t, e.allowed("ex").Status.Conditions)
	if c.Reason != techv1alpha1.ReasonCredentials || !strings.Contains(c.Message, "creds") {
		t.Fatalf("Ready = %+v", c)
	}
	if e.last.RequeueAfter != retryInterval {
		t.Fatalf("requeue = %v", e.last.RequeueAfter)
	}

	e.prov.credErr = nil
	e.tech.Err = errors.New("connection refused")
	if _, err := e.allw.Reconcile(context.Background(), req("ex")); err == nil {
		t.Fatal("want an error for retry with backoff")
	}
	c = ready(t, e.allowed("ex").Status.Conditions)
	if c.Status != metav1.ConditionFalse || c.Reason != techv1alpha1.ReasonTechnitium ||
		!strings.Contains(c.Message, "connection refused") {
		t.Fatalf("Ready = %+v", c)
	}

	e.tech.Err = technitium.ErrAuth
	if _, _ = e.allw.Reconcile(context.Background(), req("ex")); true {
		c = ready(t, e.allowed("ex").Status.Conditions)
		if c.Reason != techv1alpha1.ReasonCredentials {
			t.Fatalf("Ready = %+v", c)
		}
	}
}

func TestDomainDryRun(t *testing.T) {
	e := newEnv(t, true, srv("primary"), allowedDomain("new", "new.example.com"), allowedDomain("old", "old.example.com"))
	e.tech.Seed(technitium.Allowed, "old.example.com")

	e.reconcileAllowed("new")
	e.reconcileAllowed("old")
	expectCalls(t, e.tech)
	n, o := e.allowed("new"), e.allowed("old")
	if c := ready(t, n.Status.Conditions); c.Status != metav1.ConditionFalse || c.Reason != techv1alpha1.ReasonDryRunPending {
		t.Fatalf("new: Ready = %+v", c)
	}
	if n.Status.Domain != "" {
		t.Fatalf("a dry-run must not claim the entry: %+v", n.Status)
	}
	if c := ready(t, o.Status.Conditions); c.Status != metav1.ConditionTrue {
		t.Fatalf("old: Ready = %+v", c)
	}
	if !meta.IsStatusConditionTrue(n.Status.Conditions, techv1alpha1.ConditionDryRun) {
		t.Fatal("no DryRun condition")
	}
	if !slices.ContainsFunc(e.events(), func(s string) bool { return strings.Contains(s, "DryRun: would add") }) {
		t.Fatal("no DryRun event")
	}
	// deleting in dry-run mode sends nothing and removes the finalizer
	if err := e.c.Delete(context.Background(), e.allowed("old")); err != nil {
		t.Fatal(err)
	}
	e.reconcileAllowed("old")
	expectCalls(t, e.tech)
}

func TestDomainRejectsEmpty(t *testing.T) {
	e := newEnv(t, false, srv("primary"), allowedDomain("ex", " . "))
	e.reconcileAllowed("ex")
	if c := ready(t, e.allowed("ex").Status.Conditions); c.Reason != techv1alpha1.ReasonInvalid {
		t.Fatalf("Ready = %+v", c)
	}
	expectCalls(t, e.tech)
}

// ---- servers ----

func TestServerStatusAndNoSettingsMeansUntouched(t *testing.T) {
	e := newEnv(t, false, srv("primary"))
	e.tech.SeedBlockListURLs("https://lists.invalid/a")
	e.mustReconcileServer()
	s := e.server()
	if s.Status.Version != "15.0" || s.Status.ObservedGeneration != 1 {
		t.Fatalf("status = %+v", s.Status)
	}
	if c := ready(t, s.Status.Conditions); c.Status != metav1.ConditionTrue || c.Reason != techv1alpha1.ReasonReconciled {
		t.Fatalf("Ready = %+v", c)
	}
	expectCalls(t, e.tech)
}

func TestServerBlockListURLs(t *testing.T) {
	urls := func(u ...string) func(*techv1alpha1.TechnitiumServer) {
		return func(s *techv1alpha1.TechnitiumServer) {
			list := append([]string{}, u...) // not nil: [] clears
			s.Spec.Settings = &techv1alpha1.ServerSettings{BlockListURLs: &list}
		}
	}
	t.Run("writes only on a difference", func(t *testing.T) {
		e := newEnv(t, false, srv("primary", urls("https://a/x", "https://b/y")))
		e.mustReconcileServer()
		expectCalls(t, e.tech, "set blocklisturls https://a/x,https://b/y")
		e.tech.ResetCalls()
		e.mustReconcileServer()
		expectCalls(t, e.tech)
	})
	t.Run("empty list clears", func(t *testing.T) {
		e := newEnv(t, false, srv("primary", urls()))
		e.tech.SeedBlockListURLs("https://a/x")
		e.mustReconcileServer()
		expectCalls(t, e.tech, "set blocklisturls ")
		if got := e.tech.BlockListURLs(); len(got) != 0 {
			t.Fatalf("urls = %q", got)
		}
		e.tech.ResetCalls()
		e.mustReconcileServer()
		expectCalls(t, e.tech)
	})
	t.Run("settings without blockListUrls leave them alone", func(t *testing.T) {
		e := newEnv(t, false, srv("primary", func(s *techv1alpha1.TechnitiumServer) {
			s.Spec.Settings = &techv1alpha1.ServerSettings{}
		}))
		e.tech.SeedBlockListURLs("https://a/x")
		e.mustReconcileServer()
		expectCalls(t, e.tech)
	})
}

func TestServerAppConfigMergesPartially(t *testing.T) {
	e := newEnv(t, false, srv("primary", func(s *techv1alpha1.TechnitiumServer) {
		s.Spec.Apps = []techv1alpha1.AppConfig{
			{Name: "Query Logs (Sqlite)", Config: jsonOf(`{"maxLogDays":30,"enableLogging":true}`)},
			{Name: "Missing", Config: jsonOf(`{"a":1}`)},
			{Name: "Other", Config: jsonOf(`{"x":1}`)},
		}
	}))
	e.tech.InstallApp("Query Logs (Sqlite)", `{"enableLogging":false,"maxLogDays":0,"sqliteDbPath":"querylogs.db"}`)
	e.tech.InstallApp("Other", "")

	e.mustReconcileServer()
	expectCalls(t, e.tech, "set appconfig Query Logs (Sqlite)", "set appconfig Other")
	merged, err := decodeObject(e.tech.AppConfig("Query Logs (Sqlite)"))
	if err != nil {
		t.Fatal(err)
	}
	if merged["sqliteDbPath"] != "querylogs.db" || merged["enableLogging"] != true || merged["maxLogDays"].(interface{ String() string }).String() != "30" {
		t.Fatalf("merged = %v", merged)
	}
	// one unknown app: the others are applied, Ready names the problem
	c := ready(t, e.server().Status.Conditions)
	if c.Status != metav1.ConditionFalse || c.Reason != techv1alpha1.ReasonAppError || !strings.Contains(c.Message, `"Missing"`) {
		t.Fatalf("Ready = %+v", c)
	}
	if e.last.RequeueAfter != retryInterval {
		t.Fatalf("requeue = %v", e.last.RequeueAfter)
	}

	// second pass: nothing to write
	e.tech.ResetCalls()
	e.mustReconcileServer()
	expectCalls(t, e.tech)
}

func TestServerUnreachable(t *testing.T) {
	e := newEnv(t, false, srv("primary"))
	e.tech.Err = errors.New("connection refused")
	if err := e.reconcileServer("primary"); err == nil {
		t.Fatal("want an error")
	}
	c := ready(t, e.server().Status.Conditions)
	if c.Status != metav1.ConditionFalse || c.Reason != techv1alpha1.ReasonTechnitium {
		t.Fatalf("Ready = %+v", c)
	}
	e.tech.Err = nil
	e.prov.credErr = errors.New("no key")
	e.mustReconcileServer()
	if c := ready(t, e.server().Status.Conditions); c.Reason != techv1alpha1.ReasonCredentials {
		t.Fatalf("Ready = %+v", c)
	}
}

func TestServerForgetsDeletedServer(t *testing.T) {
	e := newEnv(t, false, srv("primary"))
	e.mustReconcileServer()
	if err := e.c.Delete(context.Background(), e.server()); err != nil {
		t.Fatal(err)
	}
	e.mustReconcileServer()
	if !slices.Equal(e.prov.forgotten, []string{"primary"}) {
		t.Fatalf("forgotten = %v", e.prov.forgotten)
	}
}

func pruneAll(s *techv1alpha1.TechnitiumServer) {
	s.Spec.PruneUnmanaged = techv1alpha1.PruneUnmanaged{AllowedDomains: true, BlockedDomains: true}
}

func TestServerPrune(t *testing.T) {
	deleting := allowedDomain("going", "going.example.com", orphan)
	deleting.Finalizers = []string{Finalizer}
	now := metav1.NewTime(time.Now())
	deleting.DeletionTimestamp = &now
	other := allowedDomain("other", "elsewhere.example.com", func(s *techv1alpha1.DomainSpec) { s.ServerRef.Name = "secondary" })
	e := newEnv(t, false, srv("primary", pruneAll), srv("secondary"),
		allowedDomain("keep", "Keep.Example.com."), deleting, other,
		blockedDomain("bad", "bad.example.org"))
	e.tech.Seed(technitium.Allowed, "keep.example.com", "going.example.com", "stray.example.com", "elsewhere.example.com")
	e.tech.Seed(technitium.Blocked, "bad.example.org", "stray.example.org")

	e.mustReconcileServer()
	expectCalls(t, e.tech,
		"delete allowed elsewhere.example.com", // another server's resource does not cover it
		"delete allowed stray.example.com",
		"delete blocked stray.example.org")
	if got := e.tech.Domains(technitium.Allowed); !slices.Equal(got, []string{"going.example.com", "keep.example.com"}) {
		t.Fatalf("allowed = %q", got)
	}
	if got := e.tech.Domains(technitium.Blocked); !slices.Equal(got, []string{"bad.example.org"}) {
		t.Fatalf("blocked = %q", got)
	}
	e.tech.ResetCalls()
	e.mustReconcileServer()
	expectCalls(t, e.tech)
}

func TestServerPruneOnlyWhatIsEnabled(t *testing.T) {
	e := newEnv(t, false, srv("primary", func(s *techv1alpha1.TechnitiumServer) {
		s.Spec.PruneUnmanaged.BlockedDomains = true
	}))
	e.tech.Seed(technitium.Allowed, "stray.example.com")
	e.tech.Seed(technitium.Blocked, "stray.example.org")
	e.mustReconcileServer()
	expectCalls(t, e.tech, "delete blocked stray.example.org")

	// and by default nothing at all
	e = newEnv(t, false, srv("primary"))
	e.tech.Seed(technitium.Allowed, "stray.example.com")
	e.mustReconcileServer()
	expectCalls(t, e.tech)
}

func TestServerNeverPrunesBeforeTheCacheIsSynced(t *testing.T) {
	e := newEnv(t, false, srv("primary", pruneAll))
	e.tech.Seed(technitium.Allowed, "precious.example.com")
	e.srv.Cache = unsynced{}
	e.mustReconcileServer()
	expectCalls(t, e.tech)
	c := ready(t, e.server().Status.Conditions)
	if c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "cache is not synced") {
		t.Fatalf("Ready = %+v", c)
	}
}

func TestServerPruneDryRun(t *testing.T) {
	e := newEnv(t, true, srv("primary", pruneAll))
	e.tech.Seed(technitium.Allowed, "stray.example.com")
	e.mustReconcileServer()
	expectCalls(t, e.tech)
	if c := ready(t, e.server().Status.Conditions); c.Reason != techv1alpha1.ReasonDryRunPending {
		t.Fatalf("Ready = %+v", c)
	}
	if !slices.ContainsFunc(e.events(), func(s string) bool { return strings.Contains(s, "DryRun: would delete unmanaged") }) {
		t.Fatal("no DryRun event")
	}
}
