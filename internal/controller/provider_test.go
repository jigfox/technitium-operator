package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	techv1alpha1 "github.com/jigfox/technitium-operator/api/v1alpha1"
)

func secret(name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func TestSecretClientProviderCredentials(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(
		secret("creds", map[string]string{"username": "admin", "password": "pw"}),
		secret("custom", map[string]string{"USER": "ops", "PASS": "pw2", "TOKEN": "tok"}),
		secret("nopass", map[string]string{"username": "admin"}),
	).Build()
	p := &SecretClientProvider{Reader: c}

	for _, tc := range []struct {
		name    string
		ref     techv1alpha1.CredentialsSecretRef
		want    string // part of the error, empty for success
		wantErr bool
	}{
		{name: "defaults", ref: techv1alpha1.CredentialsSecretRef{Name: "creds"}},
		{name: "default keys given explicitly", ref: techv1alpha1.CredentialsSecretRef{
			Name: "creds", UsernameKey: "username", PasswordKey: "password"}},
		{name: "custom keys", ref: techv1alpha1.CredentialsSecretRef{Name: "custom", UsernameKey: "USER", PasswordKey: "PASS"}},
		{name: "literal username", ref: techv1alpha1.CredentialsSecretRef{Name: "custom", Username: "admin", PasswordKey: "PASS"}},
		{name: "token", ref: techv1alpha1.CredentialsSecretRef{Name: "custom", TokenKey: "TOKEN"}},
		{name: "missing secret", ref: techv1alpha1.CredentialsSecretRef{Name: "nope"}, wantErr: true, want: `secret "nope" not found`},
		{name: "missing password key", ref: techv1alpha1.CredentialsSecretRef{Name: "nopass"}, wantErr: true, want: `no key "password"`},
		{name: "missing token key", ref: techv1alpha1.CredentialsSecretRef{Name: "creds", TokenKey: "TOKEN"}, wantErr: true, want: `no key "TOKEN"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := srv("s", func(s *techv1alpha1.TechnitiumServer) { s.Spec.CredentialsSecretRef = tc.ref })
			got, err := p.ClientFor(ctx, s)
			if tc.wantErr {
				var ce *CredentialsError
				if !errors.As(err, &ce) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want a CredentialsError containing %q", err, tc.want)
				}
				return
			}
			if err != nil || got == nil {
				t.Fatalf("client = %v, err = %v", got, err)
			}
		})
	}
}

func TestSecretClientProviderCachesPerServer(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(secret("creds", map[string]string{"username": "admin", "password": "pw"})).Build()
	p := &SecretClientProvider{Reader: c}
	s := srv("s")

	first, err := p.ClientFor(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := p.ClientFor(ctx, s); again != first {
		t.Fatal("the client (and its session) must be reused")
	}
	other, _ := p.ClientFor(ctx, srv("other"))
	if other == first {
		t.Fatal("servers must not share a client")
	}

	// rotating the password or moving the URL gives a new client
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "creds"}, &sec); err != nil {
		t.Fatal(err)
	}
	sec.Data["password"] = []byte("rotated")
	if err := c.Update(ctx, &sec); err != nil {
		t.Fatal(err)
	}
	if rotated, _ := p.ClientFor(ctx, s); rotated == first {
		t.Fatal("rotated credentials were not picked up")
	}
	moved := srv("s", func(s *techv1alpha1.TechnitiumServer) { s.Spec.URL = "http://elsewhere.invalid:5380" })
	again, _ := p.ClientFor(ctx, moved)
	if reused, _ := p.ClientFor(ctx, moved); reused != again {
		t.Fatal("the new client must be cached")
	}

	p.Forget(ctx, types.NamespacedName{Namespace: ns, Name: "s"})
	if fresh, _ := p.ClientFor(ctx, moved); fresh == again {
		t.Fatal("a forgotten server gets a new client")
	}
}

func TestSecretClientProviderStartEndsSessionsOnShutdown(t *testing.T) {
	c := fake.NewClientBuilder().WithObjects(secret("creds", map[string]string{"username": "admin", "password": "pw"})).Build()
	p := &SecretClientProvider{Reader: c}
	if _, err := p.ClientFor(context.Background(), srv("s")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- p.Start(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(p.clients) != 0 {
		t.Fatal("clients not released")
	}
}
