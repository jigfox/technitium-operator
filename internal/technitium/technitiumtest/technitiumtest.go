//go:build integration

// Package technitiumtest starts a real Technitium DNS Server in a container for
// integration tests.
package technitiumtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// DefaultImage is the Technitium image the integration tests run against;
// override it with TECHNITIUM_IMAGE.
const DefaultImage = "technitium/dns-server:15.6.2"

// Server is a running Technitium.
type Server struct {
	URL      string
	Username string
	Password string
}

// Start runs Technitium and registers its cleanup with t.
func Start(ctx context.Context, t *testing.T) *Server {
	t.Helper()
	image := os.Getenv("TECHNITIUM_IMAGE")
	if image == "" {
		image = DefaultImage
	}
	const password = "integration-test-password"
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{"5380/tcp"},
			Env:          map[string]string{"DNS_SERVER_ADMIN_PASSWORD": password},
			WaitingFor:   wait.ForHTTP("/api/status").WithPort("5380/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("starting technitium: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := ctr.MappedPort(ctx, "5380/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{URL: fmt.Sprintf("http://%s:%s", host, port.Port()), Username: "admin", Password: password}
}

// call posts form to path with the bearer token and returns the response
// object; Technitium's error statuses are test failures.
func (s *Server) call(t *testing.T, token, path string, form url.Values) json.RawMessage {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		Status       string          `json:"status"`
		ErrorMessage string          `json:"errorMessage"`
		Response     json.RawMessage `json:"response"`
		Token        string          `json:"token"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%s: %v: %s", path, err, body)
	}
	if env.Status != "ok" {
		t.Fatalf("%s: %s: %s", path, env.Status, env.ErrorMessage)
	}
	if env.Token != "" {
		return json.RawMessage(`{"token":"` + env.Token + `"}`)
	}
	return env.Response
}

func (s *Server) session(t *testing.T) string {
	t.Helper()
	var out struct{ Token string }
	if err := json.Unmarshal(s.call(t, "", "/api/user/login",
		url.Values{"user": {s.Username}, "pass": {s.Password}}), &out); err != nil {
		t.Fatal(err)
	}
	return out.Token
}

// CreateAPIToken creates a non-expiring API token for the admin account.
func (s *Server) CreateAPIToken(t *testing.T, name string) string {
	t.Helper()
	var out struct{ Token string }
	if err := json.Unmarshal(s.call(t, "", "/api/user/createToken",
		url.Values{"user": {s.Username}, "pass": {s.Password}, "tokenName": {name}}), &out); err != nil {
		t.Fatal(err)
	}
	return out.Token
}

// InstallStoreApp downloads and installs an app of Technitium's app store. That
// needs internet access from the container; the test is skipped if there is
// none, because no app ships inside the image.
func (s *Server) InstallStoreApp(t *testing.T, name string) {
	t.Helper()
	token := s.session(t)
	defer s.call(t, token, "/api/user/logout", nil)
	var store struct {
		StoreApps []struct{ Name, URL string } `json:"storeApps"`
	}
	if err := json.Unmarshal(s.call(t, token, "/api/apps/listStoreApps", nil), &store); err != nil {
		t.Skipf("the DNS app store is not reachable: %v", err)
	}
	for _, app := range store.StoreApps {
		if app.Name == name {
			s.call(t, token, "/api/apps/downloadAndInstall", url.Values{"name": {name}, "url": {app.URL}})
			return
		}
	}
	t.Skipf("app %q is not in the store (is there internet access?)", name)
}
