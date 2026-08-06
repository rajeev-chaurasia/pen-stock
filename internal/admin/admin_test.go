package admin

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/budget"
)

// usdEpsilon absorbs binary floating point slop on a decimal figure.
const usdEpsilon = 1e-9

// fakeAccounting is a fixed reading of the enforcer, so a test states
// spend directly instead of driving requests through a real budget.
type fakeAccounting struct {
	daily     map[budget.TenantID]float64
	monthly   map[budget.TenantID]float64
	committed map[budget.TenantID]float64
}

func (f fakeAccounting) Spent(t budget.TenantID) (daily, monthly float64) {
	return f.daily[t], f.monthly[t]
}

func (f fakeAccounting) Committed(t budget.TenantID) float64 { return f.committed[t] }

// oneTenant builds a server holding a single tenant with the given
// limits and usage, which is the shape most cases here need.
func oneTenant(name budget.TenantID, limits budget.Limits, daily, monthly, committed float64) *Server {
	return New(
		fakeAccounting{
			daily:     map[budget.TenantID]float64{name: daily},
			monthly:   map[budget.TenantID]float64{name: monthly},
			committed: map[budget.TenantID]float64{name: committed},
		},
		map[budget.TenantID]budget.Limits{name: limits},
	)
}

func do(t *testing.T, s *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func ptr(f float64) *float64 { return &f }

// wantJSON fails unless the response is JSON, which is the whole point
// of the mux wiring: no endpoint may fall through to a text or HTML
// default.
func wantJSON(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSON)
	}
	if body := rec.Body.String(); strings.Contains(body, "<") {
		t.Errorf("response body looks like markup, not JSON: %q", body)
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Errorf("response body is not valid JSON: %q", rec.Body.String())
	}
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding error envelope: %v (body %q)", err, rec.Body.String())
	}
	if env.Error.Message == "" || env.Error.Type == "" || env.Error.Code == "" {
		t.Errorf("incomplete error envelope: %+v", env.Error)
	}
	return env
}

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) tenantListResponse {
	t.Helper()
	var list tenantListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding tenant list: %v (body %q)", err, rec.Body.String())
	}
	return list
}

func decodeTenant(t *testing.T, rec *httptest.ResponseRecorder) tenantView {
	t.Helper()
	var view tenantView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decoding tenant: %v (body %q)", err, rec.Body.String())
	}
	return view
}

func wantRemaining(t *testing.T, field string, got, want *float64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %v, want null for an unlimited budget", field, *got)
	case want != nil && got == nil:
		t.Errorf("%s = null, want %v", field, *want)
	case want != nil && got != nil && math.Abs(*got-*want) > usdEpsilon:
		t.Errorf("%s = %v, want %v", field, *got, *want)
	}
}

func TestTenantListIsSortedByName(t *testing.T) {
	limits := budget.Limits{DailyUSD: 10, MonthlyUSD: 100}
	s := New(fakeAccounting{}, map[budget.TenantID]budget.Limits{
		"zulu":  limits,
		"alfa":  limits,
		"mike":  limits,
		"bravo": limits,
	})

	want := []string{"alfa", "bravo", "mike", "zulu"}

	// Twice: map iteration order differs per range, so a single pass
	// could pass by luck.
	for range 2 {
		list := decodeList(t, do(t, s, http.MethodGet, pathTenants))
		got := make([]string, 0, len(list.Tenants))
		for _, view := range list.Tenants {
			got = append(got, view.Name)
		}
		if !slices.Equal(got, want) {
			t.Errorf("tenant order = %v, want %v", got, want)
		}
	}
}

func TestTenantListReportsUsagePerTenant(t *testing.T) {
	acct := fakeAccounting{
		daily:     map[budget.TenantID]float64{"alfa": 4, "bravo": 1.5},
		monthly:   map[budget.TenantID]float64{"alfa": 40, "bravo": 9},
		committed: map[budget.TenantID]float64{"alfa": 1, "bravo": 0},
	}
	s := New(acct, map[budget.TenantID]budget.Limits{
		"alfa":  {DailyUSD: 10, MonthlyUSD: 100, RequestsPerMinute: 60},
		"bravo": {DailyUSD: 5, MonthlyUSD: 50, FailClosed: true},
	})

	want := map[string]tenantView{
		"alfa": {
			Limits:              limitsView{RequestsPerMinute: 60, DailyUSD: 10, MonthlyUSD: 100},
			DailySpentUSD:       4,
			MonthlySpentUSD:     40,
			CommittedUSD:        1,
			DailyRemainingUSD:   ptr(5),  // 10 - 4 - 1
			MonthlyRemainingUSD: ptr(59), // 100 - 40 - 1
		},
		"bravo": {
			Limits:              limitsView{DailyUSD: 5, MonthlyUSD: 50, FailClosed: true},
			DailySpentUSD:       1.5,
			MonthlySpentUSD:     9,
			DailyRemainingUSD:   ptr(3.5),
			MonthlyRemainingUSD: ptr(41),
		},
	}

	rec := do(t, s, http.MethodGet, pathTenants)
	wantJSON(t, rec)
	list := decodeList(t, rec)
	if len(list.Tenants) != len(want) {
		t.Fatalf("got %d tenants, want %d", len(list.Tenants), len(want))
	}

	for _, got := range list.Tenants {
		t.Run(got.Name, func(t *testing.T) {
			expected, known := want[got.Name]
			if !known {
				t.Fatalf("unexpected tenant %q in the listing", got.Name)
			}
			if got.Limits != expected.Limits {
				t.Errorf("limits = %+v, want %+v", got.Limits, expected.Limits)
			}
			if got.DailySpentUSD != expected.DailySpentUSD {
				t.Errorf("daily_spent_usd = %v, want %v", got.DailySpentUSD, expected.DailySpentUSD)
			}
			if got.MonthlySpentUSD != expected.MonthlySpentUSD {
				t.Errorf("monthly_spent_usd = %v, want %v", got.MonthlySpentUSD, expected.MonthlySpentUSD)
			}
			if got.CommittedUSD != expected.CommittedUSD {
				t.Errorf("committed_usd = %v, want %v", got.CommittedUSD, expected.CommittedUSD)
			}
			wantRemaining(t, "daily_remaining_usd", got.DailyRemainingUSD, expected.DailyRemainingUSD)
			wantRemaining(t, "monthly_remaining_usd", got.MonthlyRemainingUSD, expected.MonthlyRemainingUSD)
		})
	}
}

func TestRemainingArithmetic(t *testing.T) {
	tests := []struct {
		name        string
		limits      budget.Limits
		daily       float64
		monthly     float64
		committed   float64
		wantDaily   *float64
		wantMonthly *float64
	}{
		{
			name:        "an untouched tenant has its whole budget",
			limits:      budget.Limits{DailyUSD: 10, MonthlyUSD: 100},
			wantDaily:   ptr(10),
			wantMonthly: ptr(100),
		},
		{
			name:        "settled spend comes off both windows",
			limits:      budget.Limits{DailyUSD: 10, MonthlyUSD: 100},
			daily:       2.5,
			monthly:     30,
			wantDaily:   ptr(7.5),
			wantMonthly: ptr(70),
		},
		{
			name: "reserved spend counts against remaining, because the enforcer counts it too",
			// The enforcer admits on spent + committed + estimate, so
			// headroom that ignored reservations would be a lie.
			limits:      budget.Limits{DailyUSD: 10, MonthlyUSD: 100},
			daily:       2,
			monthly:     20,
			committed:   3,
			wantDaily:   ptr(5),
			wantMonthly: ptr(77),
		},
		{
			name:        "an exhausted budget is zero, which is what zero means here",
			limits:      budget.Limits{DailyUSD: 10, MonthlyUSD: 100},
			daily:       10,
			monthly:     100,
			wantDaily:   ptr(0),
			wantMonthly: ptr(0),
		},
		{
			name:        "documented overshoot shows as a negative rather than being hidden",
			limits:      budget.Limits{DailyUSD: 10, MonthlyUSD: 100},
			daily:       10.75,
			monthly:     104,
			wantDaily:   ptr(-0.75),
			wantMonthly: ptr(-4),
		},
		{
			name:        "the two windows are independent",
			limits:      budget.Limits{DailyUSD: 10, MonthlyUSD: 100},
			daily:       0,
			monthly:     95,
			wantDaily:   ptr(10),
			wantMonthly: ptr(5),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := oneTenant("alfa", tt.limits, tt.daily, tt.monthly, tt.committed)
			got := decodeTenant(t, do(t, s, http.MethodGet, "/admin/tenants/alfa"))

			wantRemaining(t, "daily_remaining_usd", got.DailyRemainingUSD, tt.wantDaily)
			wantRemaining(t, "monthly_remaining_usd", got.MonthlyRemainingUSD, tt.wantMonthly)
		})
	}
}

func TestUnlimitedBudgetRendersAsNullNotZero(t *testing.T) {
	// Spend is non zero throughout, so a zero here could only come from
	// the misleading representation this test exists to rule out.
	const spentDaily, spentMonthly = 7, 70

	tests := []struct {
		name          string
		limits        budget.Limits
		wantDailyNull bool
		wantMonthNull bool
	}{
		{
			name:          "no budget configured at all",
			limits:        budget.Limits{},
			wantDailyNull: true,
			wantMonthNull: true,
		},
		{
			name:          "daily capped, monthly unlimited",
			limits:        budget.Limits{DailyUSD: 10},
			wantDailyNull: false,
			wantMonthNull: true,
		},
		{
			name:          "monthly capped, daily unlimited",
			limits:        budget.Limits{MonthlyUSD: 100},
			wantDailyNull: true,
			wantMonthNull: false,
		},
		{
			name:          "both capped",
			limits:        budget.Limits{DailyUSD: 10, MonthlyUSD: 100},
			wantDailyNull: false,
			wantMonthNull: false,
		},
		{
			name:          "rate limits alone leave both budgets unlimited",
			limits:        budget.Limits{RequestsPerMinute: 60, TokensPerMinute: 100_000},
			wantDailyNull: true,
			wantMonthNull: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := oneTenant("alfa", tt.limits, spentDaily, spentMonthly, 0)
			rec := do(t, s, http.MethodGet, "/admin/tenants/alfa")

			// Decoded generically so an explicit null is distinguishable
			// from a key that was never emitted.
			var raw map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decoding tenant: %v", err)
			}

			for field, wantNull := range map[string]bool{
				"daily_remaining_usd":   tt.wantDailyNull,
				"monthly_remaining_usd": tt.wantMonthNull,
			} {
				value, present := raw[field]
				if !present {
					t.Fatalf("%s is missing from the response, so a consumer cannot tell unlimited from an old build", field)
				}
				if wantNull && value != nil {
					t.Errorf("%s = %v, want null for an unlimited budget", field, value)
				}
				if !wantNull && value == nil {
					t.Errorf("%s = null, want a number for a configured budget", field)
				}
			}

			// Spend and limits are reported regardless of whether a cap
			// exists, so an unlimited tenant is still observable.
			view := decodeTenant(t, rec)
			if view.DailySpentUSD != spentDaily || view.MonthlySpentUSD != spentMonthly {
				t.Errorf("spend = %v/%v, want %v/%v",
					view.DailySpentUSD, view.MonthlySpentUSD, spentDaily, spentMonthly)
			}
		})
	}
}

func TestSingleTenantEndpoint(t *testing.T) {
	acct := fakeAccounting{
		daily:     map[budget.TenantID]float64{"alfa": 1, "bravo": 2},
		monthly:   map[budget.TenantID]float64{"alfa": 10, "bravo": 20},
		committed: map[budget.TenantID]float64{"alfa": 0.5},
	}
	s := New(acct, map[budget.TenantID]budget.Limits{
		"alfa":  {DailyUSD: 10, MonthlyUSD: 100},
		"bravo": {DailyUSD: 20, MonthlyUSD: 200},
	})

	tests := []struct {
		name              string
		path              string
		wantName          string
		wantDailySpent    float64
		wantDailyRemains  *float64
		wantMonthRemains  *float64
		wantCommittedUSD  float64
		wantMonthlySpends float64
	}{
		{
			name:              "alfa",
			path:              "/admin/tenants/alfa",
			wantName:          "alfa",
			wantDailySpent:    1,
			wantMonthlySpends: 10,
			wantCommittedUSD:  0.5,
			wantDailyRemains:  ptr(8.5),
			wantMonthRemains:  ptr(89.5),
		},
		{
			name:              "bravo",
			path:              "/admin/tenants/bravo",
			wantName:          "bravo",
			wantDailySpent:    2,
			wantMonthlySpends: 20,
			wantDailyRemains:  ptr(18),
			wantMonthRemains:  ptr(180),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, tt.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			wantJSON(t, rec)

			got := decodeTenant(t, rec)
			if got.Name != tt.wantName {
				t.Errorf("name = %q, want %q", got.Name, tt.wantName)
			}
			if got.DailySpentUSD != tt.wantDailySpent {
				t.Errorf("daily_spent_usd = %v, want %v", got.DailySpentUSD, tt.wantDailySpent)
			}
			if got.MonthlySpentUSD != tt.wantMonthlySpends {
				t.Errorf("monthly_spent_usd = %v, want %v", got.MonthlySpentUSD, tt.wantMonthlySpends)
			}
			if got.CommittedUSD != tt.wantCommittedUSD {
				t.Errorf("committed_usd = %v, want %v", got.CommittedUSD, tt.wantCommittedUSD)
			}
			wantRemaining(t, "daily_remaining_usd", got.DailyRemainingUSD, tt.wantDailyRemains)
			wantRemaining(t, "monthly_remaining_usd", got.MonthlyRemainingUSD, tt.wantMonthRemains)

			// A single tenant response is one element of the listing, not
			// a shape of its own.
			var single, fromList map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
				t.Fatalf("decoding tenant: %v", err)
			}
			listRec := do(t, s, http.MethodGet, pathTenants)
			var wrapper struct {
				Tenants []map[string]any `json:"tenants"`
			}
			if err := json.Unmarshal(listRec.Body.Bytes(), &wrapper); err != nil {
				t.Fatalf("decoding list: %v", err)
			}
			for _, entry := range wrapper.Tenants {
				if entry["name"] == tt.wantName {
					fromList = entry
				}
			}
			if fromList == nil {
				t.Fatalf("tenant %q missing from the listing", tt.wantName)
			}
			if len(single) != len(fromList) {
				t.Errorf("single tenant has %d fields, the listing entry has %d", len(single), len(fromList))
			}
		})
	}
}

func TestUnknownTenantIs404WithEnvelope(t *testing.T) {
	s := oneTenant("alfa", budget.Limits{DailyUSD: 10}, 0, 0, 0)

	tests := []struct {
		name string
		path string
	}{
		{name: "never configured", path: "/admin/tenants/nobody"},
		{name: "case does not fold", path: "/admin/tenants/ALFA"},
		{name: "percent encoded name", path: "/admin/tenants/al%20fa"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, tt.path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
			}
			wantJSON(t, rec)

			env := decodeEnvelope(t, rec)
			if env.Error.Code != codeTenantNotFound {
				t.Errorf("code = %q, want %q", env.Error.Code, codeTenantNotFound)
			}
			if env.Error.Type != errTypeInvalidRequest {
				t.Errorf("type = %q, want %q", env.Error.Type, errTypeInvalidRequest)
			}
		})
	}
}

func TestUnknownPathIsJSONNotHTML(t *testing.T) {
	s := oneTenant("alfa", budget.Limits{DailyUSD: 10}, 0, 0, 0)

	tests := []struct {
		name string
		path string
	}{
		{name: "root", path: "/"},
		{name: "admin prefix alone", path: "/admin"},
		{name: "below a real endpoint", path: "/admin/tenants/alfa/detail"},
		{name: "trailing slash on the collection", path: "/admin/tenants/"},
		{name: "a neighbour listener's path", path: "/metrics"},
		{name: "public api path", path: "/v1/chat/completions"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, tt.path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
			}
			wantJSON(t, rec)
			decodeEnvelope(t, rec)
		})
	}

	// The plain unknown path carries the generic code, distinct from a
	// known endpoint answering about a name it does not have.
	env := decodeEnvelope(t, do(t, s, http.MethodGet, "/nope"))
	if env.Error.Code != codeNotFound {
		t.Errorf("code = %q, want %q", env.Error.Code, codeNotFound)
	}
}

func TestNonGETIs405WithAllowHeader(t *testing.T) {
	s := oneTenant("alfa", budget.Limits{DailyUSD: 10}, 0, 0, 0)

	paths := []string{pathTenants, "/admin/tenants/alfa", pathHealthz}
	methods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

	for _, path := range paths {
		for _, method := range methods {
			t.Run(method+" "+path, func(t *testing.T) {
				rec := do(t, s, method, path)
				if rec.Code != http.StatusMethodNotAllowed {
					t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
				}
				if allow := rec.Header().Get("Allow"); allow != allowedMethods {
					t.Errorf("Allow = %q, want %q", allow, allowedMethods)
				}
				wantJSON(t, rec)

				env := decodeEnvelope(t, rec)
				if env.Error.Code != codeMethodNotAllowed {
					t.Errorf("code = %q, want %q", env.Error.Code, codeMethodNotAllowed)
				}
			})
		}
	}
}

func TestHealthz(t *testing.T) {
	// No tenants configured: the probe answers about the listener, not
	// about the budget.
	s := New(fakeAccounting{}, nil)

	rec := do(t, s, http.MethodGet, pathHealthz)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	wantJSON(t, rec)

	var got healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding health: %v", err)
	}
	if got.Status != statusOK {
		t.Errorf("status = %q, want %q", got.Status, statusOK)
	}
}

func TestEmptyTenantListIsAnArrayNotNull(t *testing.T) {
	// A null here would make a consumer branch on it; an empty array
	// just iterates zero times.
	rec := do(t, New(fakeAccounting{}, nil), http.MethodGet, pathTenants)
	if body := strings.TrimSpace(rec.Body.String()); body != `{"tenants":[]}` {
		t.Errorf("body = %s, want an empty array", body)
	}
}

func TestNilAccountingDoesNotBreakTheEndpoint(t *testing.T) {
	// Budgeting is optional in this gateway, so an unwired enforcer must
	// still leave the configuration readable.
	s := New(nil, map[budget.TenantID]budget.Limits{"alfa": {DailyUSD: 10}})

	rec := do(t, s, http.MethodGet, "/admin/tenants/alfa")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	got := decodeTenant(t, rec)
	if got.DailySpentUSD != 0 || got.CommittedUSD != 0 {
		t.Errorf("spend = %v/%v, want zeros", got.DailySpentUSD, got.CommittedUSD)
	}
	wantRemaining(t, "daily_remaining_usd", got.DailyRemainingUSD, ptr(10))
}

func TestLimitsAreCopiedFromTheCaller(t *testing.T) {
	limits := map[budget.TenantID]budget.Limits{"alfa": {DailyUSD: 10}}
	s := New(fakeAccounting{}, limits)

	// An operator reading this endpoint must see what the enforcer was
	// built with, not whatever the caller's map holds now.
	limits["alfa"] = budget.Limits{DailyUSD: 999}
	limits["intruder"] = budget.Limits{DailyUSD: 1}

	list := decodeList(t, do(t, s, http.MethodGet, pathTenants))
	if len(list.Tenants) != 1 {
		t.Fatalf("got %d tenants, want 1", len(list.Tenants))
	}
	if list.Tenants[0].Limits.DailyUSD != 10 {
		t.Errorf("daily_usd = %v, want 10", list.Tenants[0].Limits.DailyUSD)
	}
}

// allowedFields is every JSON key this API may emit. The test below
// fails on an addition as loudly as on a removal, which is the point:
// this handler reads operator data, and a field arriving unnoticed is
// exactly the accident worth catching.
var allowedFields = map[string]bool{
	// listing
	"tenants": true,
	// tenant
	"name":                  true,
	"limits":                true,
	"daily_spent_usd":       true,
	"monthly_spent_usd":     true,
	"committed_usd":         true,
	"daily_remaining_usd":   true,
	"monthly_remaining_usd": true,
	// limits
	"requests_per_minute": true,
	"tokens_per_minute":   true,
	"daily_usd":           true,
	"monthly_usd":         true,
	"fail_closed":         true,
	// error envelope
	"error":   true,
	"message": true,
	"type":    true,
	"code":    true,
	// health
	"status": true,
}

// forbiddenSubstrings are words that would signal a secret or user
// content leaking into an operator response. "token" is deliberately
// absent: tokens_per_minute is a rate, not text.
var forbiddenSubstrings = []string{
	"key", "secret", "bearer", "authorization", "password",
	"credential", "prompt", "completion", "sk-",
}

func collectFields(v any, into map[string]bool) {
	switch node := v.(type) {
	case map[string]any:
		for field, child := range node {
			into[field] = true
			collectFields(child, into)
		}
	case []any:
		for _, child := range node {
			collectFields(child, into)
		}
	}
}

func TestResponsesCarryOnlyTheDocumentedSchema(t *testing.T) {
	// Innocuous names, so anything alarming found in a body came from
	// the handler rather than from the fixture.
	acct := fakeAccounting{
		daily:     map[budget.TenantID]float64{"alfa": 1, "bravo": 2},
		monthly:   map[budget.TenantID]float64{"alfa": 10, "bravo": 20},
		committed: map[budget.TenantID]float64{"alfa": 0.5},
	}
	s := New(acct, map[budget.TenantID]budget.Limits{
		"alfa":  {DailyUSD: 10, MonthlyUSD: 100, RequestsPerMinute: 60, TokensPerMinute: 1000},
		"bravo": {FailClosed: true},
	})

	requests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "listing", method: http.MethodGet, path: pathTenants},
		{name: "one tenant", method: http.MethodGet, path: "/admin/tenants/alfa"},
		{name: "unlimited tenant", method: http.MethodGet, path: "/admin/tenants/bravo"},
		{name: "unknown tenant", method: http.MethodGet, path: "/admin/tenants/nobody"},
		{name: "unknown path", method: http.MethodGet, path: "/nope"},
		{name: "method not allowed", method: http.MethodPost, path: pathTenants},
		{name: "health", method: http.MethodGet, path: pathHealthz},
	}

	seen := make(map[string]bool)
	for _, req := range requests {
		t.Run(req.name, func(t *testing.T) {
			rec := do(t, s, req.method, req.path)
			wantJSON(t, rec)

			body := strings.ToLower(rec.Body.String())
			for _, banned := range forbiddenSubstrings {
				if strings.Contains(body, banned) {
					t.Errorf("response contains %q, which must never reach an operator endpoint: %s", banned, body)
				}
			}

			var decoded any
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			fields := make(map[string]bool)
			collectFields(decoded, fields)
			for field := range fields {
				if !allowedFields[field] {
					t.Errorf("undocumented field %q in the response", field)
				}
				seen[field] = true
			}
		})
	}

	// The other direction: a field that quietly stopped being emitted is
	// as much a schema change as one that appeared.
	for field := range allowedFields {
		if !seen[field] {
			t.Errorf("documented field %q was never emitted by any endpoint", field)
		}
	}
}
