package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/httperr"
)

// wantSharedEnvelope fails unless the recorded response is byte for byte
// what the shared writer produces for the same envelope.
//
// The decode and re-encode is the whole trick: a renamed field, an extra
// field, a different nesting, or a lost trailing newline all survive the
// decode into httperr.Envelope but cannot be reproduced by re-encoding
// it, so the comparison fails. This is the check that a second copy of
// the envelope in this package would eventually break.
func wantSharedEnvelope(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	var env httperr.Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding error envelope: %v (body %q)", err, rec.Body.String())
	}
	if env.Error.Message == "" || env.Error.Type == "" || env.Error.Code == "" {
		t.Fatalf("incomplete error envelope: %+v", env.Error)
	}

	shared := httptest.NewRecorder()
	httperr.WriteError(shared, rec.Code, env.Error.Message, env.Error.Type, env.Error.Code)

	if got, want := rec.Body.String(), shared.Body.String(); got != want {
		t.Errorf("admin body =\n%q\nshared writer produces\n%q", got, want)
	}
	if got, want := rec.Header().Get("Content-Type"), shared.Header().Get("Content-Type"); got != want {
		t.Errorf("admin Content-Type = %q, shared writer sets %q", got, want)
	}
}

// TestWriteErrorMatchesSharedWriter covers the package-local helper on
// its own, without a mux in the way.
func TestWriteErrorMatchesSharedWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusNotFound, "tenant is not configured", codeTenantNotFound)

	wantSharedEnvelope(t, rec)

	const want = `{"error":{"message":"tenant is not configured","type":"invalid_request_error","code":"tenant_not_found"}}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body =\n%q\nwant\n%q", got, want)
	}
}

// TestEveryErrorPathEmitsTheSharedEnvelope walks the real mux, since a
// handler that hand rolled its own error body would slip past a test
// that only exercises the helper.
func TestEveryErrorPathEmitsTheSharedEnvelope(t *testing.T) {
	s := New(nil, nil)

	for _, tc := range []struct {
		name   string
		method string
		target string
	}{
		{"unknown tenant", http.MethodGet, "/admin/tenants/nope"},
		{"unknown path", http.MethodGet, "/admin/nope"},
		{"method not allowed", http.MethodPost, "/admin/tenants"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))
			wantSharedEnvelope(t, rec)
		})
	}
}
