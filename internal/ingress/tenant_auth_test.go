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
// Test keys are built from repeated characters rather than written as
// random looking literals. They only need to clear the minimum length,
// and a high entropy string in a test file trips secret scanners, which
// trains everyone to ignore them.
var (
	testTenantKey = strings.Repeat("t", 24)
	testAnonKey   = strings.Repeat("a", 24)
	testOtherKey  = strings.Repeat("z", 24)
)

func TestTenantKeysAuthenticateOnTheirOwn(t *testing.T) {
	tenantKey := testTenantKey
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
	if got := postWithKey(t, ts.URL, testOtherKey).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status with a wrong key = %d, want 401", got)
	}
	if got := postWithKey(t, ts.URL, tenantKey).StatusCode; got != http.StatusOK {
		t.Errorf("status with the tenant key = %d, want 200", got)
	}
}

func TestTenantAndAnonymousKeysCoexist(t *testing.T) {
	anonKey, tenantKey := testAnonKey, testTenantKey
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
	if got := postWithKey(t, ts.URL, testOtherKey).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status with an unknown key = %d, want 401", got)
	}
}

func TestHealthzStaysOpenWithTenantKeys(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := ingress.NewServer(defaultCfg(),
		map[string]providers.Provider{}, log,
		ingress.WithTenantKeys(map[string][]string{"acme": {testTenantKey}}),
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
