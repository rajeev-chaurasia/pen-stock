package ingress_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

func okProvider(name string) *fakeProvider {
	return &fakeProvider{
		name: name,
		chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{Body: []byte(`{}`)}, nil
		},
	}
}

func postWithKey(t *testing.T, url, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// A deployment may configure tenant keys and no anonymous client keys.
// If those keys were not registered with the auth layer, such a gateway
// would come up with an empty key set and serve everyone, which is the
// opposite of what configuring tenants asks for.
func TestTenantKeysAuthenticateOnTheirOwn(t *testing.T) {
	const tenantKey = "tenant-key-0123456789abcdef"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := ingress.NewServer(defaultCfg(),
		map[string]providers.Provider{"m": okProvider("p")}, log,
		ingress.WithClientKeys(nil),
		ingress.WithTenantKeys(map[string][]string{"acme": {tenantKey}}),
	)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	if !srv.RequiresAuth() {
		t.Fatal("RequiresAuth() = false with tenant keys configured; the gateway would be open")
	}
	if got := postWithKey(t, ts.URL, "").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status without a key = %d, want 401", got)
	}
	if got := postWithKey(t, ts.URL, "wrong-key-0123456789abcdef").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status with a wrong key = %d, want 401", got)
	}
	if got := postWithKey(t, ts.URL, tenantKey).StatusCode; got != http.StatusOK {
		t.Errorf("status with the tenant key = %d, want 200", got)
	}
}

func TestTenantAndAnonymousKeysCoexist(t *testing.T) {
	const (
		anonKey   = "anon-key-0123456789abcdef"
		tenantKey = "acme-key-0123456789abcdef"
	)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := ingress.NewServer(defaultCfg(),
		map[string]providers.Provider{"m": okProvider("p")}, log,
		ingress.WithClientKeys([]string{anonKey}),
		ingress.WithTenantKeys(map[string][]string{"acme": {tenantKey}}),
	)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	for _, key := range []string{anonKey, tenantKey} {
		if got := postWithKey(t, ts.URL, key).StatusCode; got != http.StatusOK {
			t.Errorf("status with key %q = %d, want 200", key, got)
		}
	}
	if got := postWithKey(t, ts.URL, "neither-0123456789abcdef").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status with an unknown key = %d, want 401", got)
	}
}

func TestHealthzStaysOpenWithTenantKeys(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := ingress.NewServer(defaultCfg(),
		map[string]providers.Provider{}, log,
		ingress.WithTenantKeys(map[string][]string{"acme": {"acme-key-0123456789abcdef"}}),
	)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200 without a credential", resp.StatusCode)
	}
}
