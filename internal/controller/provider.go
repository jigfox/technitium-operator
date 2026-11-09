package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	techv1alpha1 "github.com/jigfox/technitium-operator/api/v1alpha1"
	"github.com/jigfox/technitium-operator/internal/technitium"
)

// SecretClientProvider builds one technitium.HTTPClient per TechnitiumServer
// from the Secret its credentialsSecretRef names, and keeps it (with its login
// session) until the URL or the credentials change, the server is deleted or
// the manager stops. The Secret is read on every call, uncached, so the
// operator needs only get on Secrets and rotated credentials apply at once.
type SecretClientProvider struct {
	// Reader must read Secrets without an informer (mgr.GetAPIReader()).
	Reader client.Reader
	// HTTPClient is shared by all servers; nil uses a default with a timeout.
	HTTPClient *http.Client

	mu      sync.Mutex
	clients map[types.NamespacedName]*cachedClient
}

type cachedClient struct {
	fingerprint [sha256.Size]byte
	client      technitium.Client
}

// ClientFor implements ClientProvider.
func (p *SecretClientProvider) ClientFor(ctx context.Context,
	server *techv1alpha1.TechnitiumServer) (technitium.Client, error) {
	cred, err := p.credentials(ctx, server)
	if err != nil {
		return nil, &CredentialsError{Err: err}
	}
	fp := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%s", server.Spec.URL, cred.Username, cred.Password, cred.Token))
	key := client.ObjectKeyFromObject(server)

	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[key]; ok {
		if c.fingerprint == fp {
			return c.client, nil
		}
		p.closeLater(c.client)
	}
	if p.clients == nil {
		p.clients = map[types.NamespacedName]*cachedClient{}
	}
	c := &cachedClient{fingerprint: fp, client: technitium.NewHTTPClient(server.Spec.URL, cred, p.HTTPClient)}
	p.clients[key] = c
	return c.client, nil
}

func (p *SecretClientProvider) credentials(ctx context.Context,
	server *techv1alpha1.TechnitiumServer) (technitium.Credentials, error) {
	ref := server.Spec.CredentialsSecretRef
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: server.Namespace, Name: ref.Name}
	if err := p.Reader.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return technitium.Credentials{}, fmt.Errorf("secret %q not found", ref.Name)
		}
		return technitium.Credentials{}, fmt.Errorf("reading secret %q: %w", ref.Name, err)
	}
	get := func(k string) (string, error) {
		v, ok := secret.Data[k]
		if !ok || len(v) == 0 {
			return "", fmt.Errorf("secret %q has no key %q", ref.Name, k)
		}
		return string(v), nil
	}
	if ref.TokenKey != "" {
		tok, err := get(ref.TokenKey)
		return technitium.Credentials{Token: tok}, err
	}
	cred := technitium.Credentials{Username: ref.Username}
	var err error
	if cred.Username == "" {
		if cred.Username, err = get(orDefault(ref.UsernameKey, "username")); err != nil {
			return cred, err
		}
	}
	cred.Password, err = get(orDefault(ref.PasswordKey, "password"))
	return cred, err
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// closeLater ends the session of a replaced client without blocking the caller.
func (p *SecretClientProvider) closeLater(c technitium.Client) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	}()
}

// Forget implements ClientProvider.
func (p *SecretClientProvider) Forget(_ context.Context, key types.NamespacedName) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[key]; ok {
		p.closeLater(c.client)
		delete(p.clients, key)
	}
}

// Start implements manager.Runnable: when the manager stops, every session is
// ended so none is left behind at the servers.
func (p *SecretClientProvider) Start(ctx context.Context) error {
	<-ctx.Done()
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	var wg sync.WaitGroup
	for key, c := range p.clients {
		wg.Go(func() {
			if err := c.client.Close(closeCtx); err != nil {
				logf.Log.Error(err, "Ending the Technitium session failed", "server", key)
			}
		})
	}
	wg.Wait()
	p.clients = nil
	return nil
}

// IsCredentialsError reports whether err came from unusable credentials, or
// Technitium refused them.
func IsCredentialsError(err error) bool {
	var ce *CredentialsError
	return errors.As(err, &ce) || errors.Is(err, technitium.ErrAuth)
}
