package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/obs"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// The tenant key is the only credential in these tests. It is long
// enough to pass the config loader's minimum, which is what a real
// deployment key has to clear too.
const bootTenantKey = "acme-key-0123456789abcdef"

// upstreamCall is what the fake backend saw. The rename a route performs
// is invisible from the client side, so the only way to prove it
// happened is to record what the provider was actually asked for.
type upstreamCall struct {
	path  string
	auth  string
	model string
}

type fakeUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	calls []upstreamCall
	// held, when set, parks the next call until release is closed, and
	// announces its arrival on entered. That is how a request is made to
	// be genuinely in flight at a chosen moment, without a sleep deciding
	// whether the test is testing anything.
	held    bool
	release <-chan struct{}
	entered chan struct{}
}

// holdNext parks the next call the upstream receives. The returned
// channel closes once that call has arrived.
func (u *fakeUpstream) holdNext(release <-chan struct{}) <-chan struct{} {
	entered := make(chan struct{})
	u.mu.Lock()
	defer u.mu.Unlock()
	u.held = true
	u.release = release
	u.entered = entered
	return entered
}

// newFakeUpstream serves the OpenAI chat wire on loopback. Nothing in
// this package may reach a real provider: a test that needs the internet
// to pass is a test that fails in CI for reasons that have nothing to do
// with the code.
func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	up := &fakeUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &envelope)

		up.mu.Lock()
		up.calls = append(up.calls, upstreamCall{
			path:  r.URL.Path,
			auth:  r.Header.Get("Authorization"),
			model: envelope.Model,
		})
		// Consumed under the lock, so exactly one call is ever parked.
		held, release, entered := up.held, up.release, up.entered
		up.held, up.release, up.entered = false, nil, nil
		up.mu.Unlock()

		if held {
			close(entered)
			<-release
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamAnswer)
	}))
	t.Cleanup(up.Close)
	return up
}

func (u *fakeUpstream) seen() []upstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamCall(nil), u.calls...)
}

// upstreamAnswer carries a usage block on purpose. Without one there is
// nothing to price, and the settlement assertions below would pass
// against a gateway that bills nothing.
const upstreamAnswer = `{"id":"chatcmpl-boot","object":"chat.completion","model":"gpt-4o-mini",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

// bootConfig writes a config file the way an operator would and loads it
// through the real loader, so defaults, validation and ${VAR} expansion
// are all in the path this test exercises.
//
// Both listeners take port 0. Other things run on the machines this is
// developed on, and a test that pins 8080 fails for whoever happens to
// be running the gateway at the time.
func bootConfig(t *testing.T, dir, upstreamURL string) *config.Config {
	t.Helper()
	body := fmt.Sprintf(`
server:
  listen: "127.0.0.1:0"
  admin_listen: "127.0.0.1:0"

auth:
  tenants:
    - name: acme
      keys: ["%s"]
      daily_usd: 5.00
      monthly_usd: 50.00

accounting:
  ledger_path: '%s'
  store_path: '%s'

providers:
  - name: primary
    kind: openai
    base_url: "%s/v1"
    api_key: upstream-test-key

routes:
  - model: auto
    provider: primary
    provider_models:
      primary: gpt-4o-mini
`,
		bootTenantKey,
		filepath.ToSlash(filepath.Join(dir, "ledger.jsonl")),
		filepath.ToSlash(filepath.Join(dir, "budget.db")),
		upstreamURL,
	)

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// boundAddr reports the address a server actually got.
//
// ListenAndServe binds the port itself and never says which one it
// chose, so with port 0 there is no other way to find out. BaseContext
// is handed the real listener once per Serve, which is exactly the hook
// needed and costs the server nothing.
func boundAddr(srv *http.Server) <-chan net.Addr {
	addrCh := make(chan net.Addr, 1)
	srv.BaseContext = func(l net.Listener) context.Context {
		addrCh <- l.Addr()
		return context.Background()
	}
	return addrCh
}

// errServeHung stands in when serve never returned, so a hang reads as a
// hang rather than as a nil error that looks like a clean drain.
var errServeHung = errors.New("serve did not return after the context was cancelled")

type bootedGateway struct {
	gatewayAddr string
	adminAddr   string
	ledgerPath  string
	client      *http.Client
	stop        context.CancelFunc
	served      <-chan error

	// once makes shutdown safe to call from both the test body and the
	// cleanup, so whichever runs second reads the same answer instead of
	// waiting forever on a channel that was already drained.
	once sync.Once
	err  error
}

func (b *bootedGateway) shutdown() error {
	b.once.Do(func() {
		b.stop()
		select {
		case b.err = <-b.served:
		case <-time.After(shutdownGrace + 5*time.Second):
			b.err = errServeHung
		}
	})
	return b.err
}

// bootGateway assembles the gateway the way run does and starts it.
//
// The ingress option list is copied from run rather than shared, because
// run also parses flags, installs signal handlers and sets up tracing,
// none of which a test can drive. That copy is the one thing here that
// can drift from production; everything downstream of it is the real
// code.
func bootGateway(t *testing.T, cfg *config.Config) *bootedGateway {
	t.Helper()
	log := slog.New(slog.DiscardHandler)

	provs, err := providers.BuildAll(cfg.Providers)
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	routes, err := buildRoutes(cfg, provs)
	if err != nil {
		t.Fatalf("buildRoutes: %v", err)
	}
	acct, err := buildAccounting(cfg, log)
	if err != nil {
		t.Fatalf("buildAccounting: %v", err)
	}
	// Registered before anything opens a listener so the SQLite file is
	// closed before t.TempDir tries to remove the directory holding it,
	// which on Windows fails while the file is open.
	t.Cleanup(acct.shutdown)

	metrics := obs.NewMetrics()
	// The binary's own assembly, not a copy of it: an option added to
	// buildGateway and not here would otherwise go untested.
	gateway := buildGateway(cfg, routes, acct, metrics, log)

	srv := gatewayServer(cfg, gateway)
	adminSrv := adminServer(cfg, metrics, acct)
	gatewayAddrCh := boundAddr(srv)
	adminAddrCh := boundAddr(adminSrv)

	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, log, srv, adminSrv) }()

	booted := &bootedGateway{
		gatewayAddr: (<-gatewayAddrCh).String(),
		adminAddr:   (<-adminAddrCh).String(),
		ledgerPath:  cfg.Accounting.LedgerPath,
		// Keep-alives off so no idle connection outlives the test and
		// holds the drain open.
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{DisableKeepAlives: true},
		},
		stop:   stop,
		served: served,
	}
	// The listeners must not outlive the test even when it fails before
	// reaching the drain assertion.
	t.Cleanup(func() { _ = booted.shutdown() })
	return booted
}

func (b *bootedGateway) get(t *testing.T, addr, path, key string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", path, err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return b.do(t, req)
}

func (b *bootedGateway) post(t *testing.T, addr, path, key, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return b.do(t, req)
}

// requestResult carries a failure back instead of reporting it, because
// the request it describes is issued from a goroutine and only the test
// goroutine may call t.Fatal.
type requestResult struct {
	status int
	body   string
	err    error
}

// chat posts a completion as the configured tenant and never touches
// testing.T, so it is safe to call from a goroutine.
func (b *bootedGateway) chat(body string) requestResult {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+b.gatewayAddr+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return requestResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bootTenantKey)

	resp, err := b.client.Do(req)
	if err != nil {
		return requestResult{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	return requestResult{status: resp.StatusCode, body: string(got), err: err}
}

func (b *bootedGateway) do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", req.URL, err)
	}
	return resp.StatusCode, string(body)
}

// The gap this closes: everything above was covered a piece at a time,
// and nothing proved the pieces fit. A gateway that loads its config,
// builds its providers, binds both listeners and answers a real request
// is the claim the binary makes, and until this test it was checked by
// hand and only when someone remembered to.
func TestGatewayBootsFromAConfigFileAndServesBothListeners(t *testing.T) {
	dir := t.TempDir()
	upstream := newFakeUpstream(t)
	cfg := bootConfig(t, dir, upstream.URL)
	gw := bootGateway(t, cfg)

	// Health stays open so a load balancer needs no credential.
	t.Run("health needs no key", func(t *testing.T) {
		if status, body := gw.get(t, gw.gatewayAddr, "/healthz", ""); status != http.StatusOK {
			t.Errorf("GET /healthz = %d %s, want 200", status, body)
		}
	})

	// Tenant keys are the only credentials this config declares. If
	// tenantKeys stopped reaching the ingress, this listener would answer
	// anyone who found it and spend the configured provider key.
	t.Run("an unkeyed caller is refused", func(t *testing.T) {
		status, body := gw.get(t, gw.gatewayAddr, "/v1/models", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("GET /v1/models unauthenticated = %d %s, want 401", status, body)
		}
	})

	t.Run("the route is served under its own name", func(t *testing.T) {
		status, body := gw.get(t, gw.gatewayAddr, "/v1/models", bootTenantKey)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/models = %d %s, want 200", status, body)
		}
		if !strings.Contains(body, `"auto"`) {
			t.Errorf("model list = %s, want the configured route \"auto\"", body)
		}
	})

	t.Run("a chat request reaches the provider and comes back whole", func(t *testing.T) {
		status, body := gw.post(t, gw.gatewayAddr, "/v1/chat/completions", bootTenantKey,
			`{"model":"auto","messages":[{"role":"user","content":"hello there"}]}`)
		if status != http.StatusOK {
			t.Fatalf("POST /v1/chat/completions = %d %s, want 200", status, body)
		}
		// The gateway forwards the upstream body untouched, so a field it
		// does not model survives the trip.
		if body != upstreamAnswer {
			t.Errorf("response body was rewritten:\n got %s\nwant %s", body, upstreamAnswer)
		}

		calls := upstream.seen()
		if len(calls) != 1 {
			t.Fatalf("upstream saw %d calls, want exactly 1", len(calls))
		}
		if calls[0].path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", calls[0].path)
		}
		if calls[0].auth != "Bearer upstream-test-key" {
			t.Errorf("upstream Authorization = %q, want the configured provider key", calls[0].auth)
		}
		// The caller asked for "auto". Sending that name upstream is how a
		// renamed route 404s forever against a provider that never heard
		// of it.
		if calls[0].model != "gpt-4o-mini" {
			t.Errorf("upstream was asked for %q, want the route's upstream model gpt-4o-mini", calls[0].model)
		}
	})

	t.Run("the admin listener reports metrics", func(t *testing.T) {
		status, body := gw.get(t, gw.adminAddr, "/metrics", "")
		if status != http.StatusOK {
			t.Fatalf("GET /metrics = %d, want 200", status)
		}
		if !strings.Contains(body, "penstock_requests_total") {
			t.Fatalf("/metrics carries no penstock series:\n%s", firstLines(body, 20))
		}
		// The metrics sink handed to the ingress and the one mounted on
		// this listener have to be the same registry, or an operator reads
		// a dashboard of zeros off a gateway that is serving traffic.
		if usd := seriesValue(body, "penstock_cost_usd_total"); usd <= 0 {
			t.Errorf("penstock_cost_usd_total = %v, want a real cost: the request path and this handler are not sharing metrics", usd)
		}
	})

	// The admin API and the request path have to read and write the same
	// counters. Giving the API its own enforcer would let it report
	// balances nobody is enforcing.
	t.Run("the tenant API reports what was spent", func(t *testing.T) {
		status, body := gw.get(t, gw.adminAddr, "/admin/tenants", "")
		if status != http.StatusOK {
			t.Fatalf("GET /admin/tenants = %d %s, want 200", status, body)
		}
		var got struct {
			Tenants []struct {
				Name          string  `json:"name"`
				DailySpentUSD float64 `json:"daily_spent_usd"`
				Limits        struct {
					DailyUSD float64 `json:"daily_usd"`
				} `json:"limits"`
			} `json:"tenants"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if len(got.Tenants) != 1 || got.Tenants[0].Name != "acme" {
			t.Fatalf("tenants = %s, want the one configured tenant acme", body)
		}
		if got.Tenants[0].Limits.DailyUSD != 5 {
			t.Errorf("daily_usd limit = %v, want the configured 5", got.Tenants[0].Limits.DailyUSD)
		}
		if got.Tenants[0].DailySpentUSD <= 0 {
			t.Errorf("daily_spent_usd = %v after a settled request, want a real figure",
				got.Tenants[0].DailySpentUSD)
		}
	})

	// The ledger is the row-level record behind the totals above. An
	// aliased route priced against the caller's name finds nothing in a
	// table keyed by vendor, and every row reads $0.00 while real money
	// is spent.
	t.Run("the cost ledger records a priced row", func(t *testing.T) {
		row := lastLedgerRow(t, gw.ledgerPath)
		if row.Tenant != "acme" {
			t.Errorf("ledger tenant = %q, want acme", row.Tenant)
		}
		if row.Model != "gpt-4o-mini" {
			t.Errorf("ledger model = %q, want the upstream model, not the route alias", row.Model)
		}
		if row.USD <= 0 {
			t.Errorf("ledger usd = %v, want a real cost for a model the price table knows", row.USD)
		}
		if row.PriceVersion <= 0 {
			t.Errorf("ledger price_version = %d, want the version that priced the row", row.PriceVersion)
		}
	})

	// Everything the tenant API does not recognise falls through to it
	// rather than to net/http's HTML default, so an operator's tooling
	// gets one content type from this listener.
	t.Run("an unknown admin path answers in JSON", func(t *testing.T) {
		status, body := gw.get(t, gw.adminAddr, "/not-a-thing", "")
		if status != http.StatusNotFound {
			t.Errorf("GET /not-a-thing = %d, want 404", status)
		}
		if !strings.HasPrefix(strings.TrimSpace(body), "{") {
			t.Errorf("admin 404 body = %q, want JSON", body)
		}
	})

	// Operator surfaces stay off the caller's listener: token spend and
	// latency profiles are not a caller's business.
	t.Run("the caller listener carries no admin surface", func(t *testing.T) {
		if status, _ := gw.get(t, gw.gatewayAddr, "/metrics", bootTenantKey); status == http.StatusOK {
			t.Error("GET /metrics on the caller listener = 200, want it served only on the admin listener")
		}
		if status, _ := gw.get(t, gw.gatewayAddr, "/admin/tenants", bootTenantKey); status == http.StatusOK {
			t.Error("GET /admin/tenants on the caller listener = 200, want it served only on the admin listener")
		}
	})

	// This is what draining means, and it is the whole reason shutdown is
	// graceful rather than a close. A completion in progress can be
	// seconds of an answer a caller is already paying for; severing it on
	// a deploy turns every rollout into a wave of truncated responses,
	// and the gateway would still look healthy from the outside.
	t.Run("a cancelled context finishes an in flight request and then closes both listeners", func(t *testing.T) {
		release := make(chan struct{})
		entered := upstream.holdNext(release)

		inFlight := make(chan requestResult, 1)
		go func() { inFlight <- gw.chat(`{"model":"auto","messages":[{"role":"user","content":"drain me"}]}`) }()

		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the upstream never received the request meant to be in flight")
		}

		// Cancel with the request parked upstream, then wait until the
		// listener has actually stopped accepting. Only then is the drain
		// demonstrably under way, so releasing the upstream here proves
		// the request survived it rather than beating it.
		gw.stop()
		waitUntilRefused(t, gw.gatewayAddr)
		close(release)

		select {
		case got := <-inFlight:
			if got.err != nil {
				t.Fatalf("the in flight request was severed by the drain: %v", got.err)
			}
			if got.status != http.StatusOK {
				t.Errorf("the in flight request finished %d, want 200", got.status)
			}
			if got.body != upstreamAnswer {
				t.Errorf("the in flight response was truncated:\n got %s\nwant %s", got.body, upstreamAnswer)
			}
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Fatal("the in flight request never completed")
		}

		// A signal is how this gateway is meant to stop, so it is not a
		// failure. Reporting one would make every restart look like a
		// crash to whatever supervises the process.
		if err := gw.shutdown(); err != nil {
			t.Fatalf("serve after cancellation = %v, want nil", err)
		}

		for name, addr := range map[string]string{"caller": gw.gatewayAddr, "admin": gw.adminAddr} {
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err == nil {
				_ = conn.Close()
				t.Errorf("the %s listener still accepts connections after the drain", name)
			}
		}
	})
}

// waitUntilRefused blocks until addr stops accepting connections, which
// is the observable moment Shutdown has closed the listener.
func waitUntilRefused(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the listener was still accepting connections long after the context was cancelled")
}

// A listener that cannot bind has to be reported. Swallowing it would
// leave the process alive and apparently healthy while the port an
// operator is pointing traffic at answers nothing.
func TestServeReportsAListenerThatCannotBind(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("take a port: %v", err)
	}
	defer func() { _ = taken.Close() }()

	srv := &http.Server{Addr: taken.Addr().String(), ReadHeaderTimeout: time.Second}
	adminSrv := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}
	// serve leaves the surviving listener to the exiting process, so the
	// test closes it rather than leaking a port for the rest of the run.
	defer func() { _ = adminSrv.Close() }()

	done := make(chan error, 1)
	go func() { done <- serve(context.Background(), slog.New(slog.DiscardHandler), srv, adminSrv) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve = nil for a port that was already held")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve hung instead of reporting the bind failure")
	}
}

// firstLines trims a long body down to something a failure message can
// carry without burying the rest of the output.
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// seriesValue reads the value of the first sample of a Prometheus
// series, or -1 when the series is absent. Labels are matched loosely on
// purpose: the assertion is about the number, not the label order the
// exposition format happens to use.
func seriesValue(body, name string) float64 {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, name+"{") && line != name {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		return v
	}
	return -1
}

type ledgerRow struct {
	Tenant       string  `json:"tenant"`
	Model        string  `json:"model"`
	USD          float64 `json:"usd"`
	PriceVersion int     `json:"price_version"`
}

func lastLedgerRow(t *testing.T, path string) ledgerRow {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- the path is the test's own temp file
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer func() { _ = f.Close() }()

	var last string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			last = line
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if last == "" {
		t.Fatal("the cost ledger is empty after a settled request")
	}
	var row ledgerRow
	if err := json.Unmarshal([]byte(last), &row); err != nil {
		t.Fatalf("decode ledger row %s: %v", last, err)
	}
	return row
}
