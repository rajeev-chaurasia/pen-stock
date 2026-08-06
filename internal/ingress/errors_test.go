package ingress

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
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
		t.Errorf("ingress body =\n%q\nshared writer produces\n%q", got, want)
	}
	if got, want := rec.Header().Get("Content-Type"), shared.Header().Get("Content-Type"); got != want {
		t.Errorf("ingress Content-Type = %q, shared writer sets %q", got, want)
	}
}

// TestWriteErrorJSONMatchesSharedWriter covers the package-local helper
// on its own, without a mux in the way.
func TestWriteErrorJSONMatchesSharedWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErrorJSON(rec, http.StatusTooManyRequests,
		"upstream rate limit reached", errTypeRateLimit, "rate_limited")

	wantSharedEnvelope(t, rec)

	const want = `{"error":{"message":"upstream rate limit reached","type":"rate_limit_error","code":"rate_limited"}}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body =\n%q\nwant\n%q", got, want)
	}
}

// TestStreamErrorFrameMatchesSharedEnvelope pins the streaming path,
// which marshals the envelope itself instead of calling the writer, so
// its frame has to be proven equal separately.
func TestStreamErrorFrameMatchesSharedEnvelope(t *testing.T) {
	local, err := json.Marshal(errorEnvelope{Error: errorBody{
		Message: "upstream stream ended before completion",
		Type:    errTypeAPI,
		Code:    "stream_truncated",
	}})
	if err != nil {
		t.Fatalf("marshal local envelope: %v", err)
	}

	shared, err := json.Marshal(httperr.Envelope{Error: httperr.Body{
		Message: "upstream stream ended before completion",
		Type:    httperr.TypeAPI,
		Code:    "stream_truncated",
	}})
	if err != nil {
		t.Fatalf("marshal shared envelope: %v", err)
	}

	if string(local) != string(shared) {
		t.Errorf("stream frame =\n%s\nshared envelope =\n%s", local, shared)
	}
}

// TestGatewayErrorPathsEmitTheSharedEnvelope walks the real handler
// chain, since a path that hand rolled its own error body would slip
// past a test that only exercises the writer.
func TestGatewayErrorPathsEmitTheSharedEnvelope(t *testing.T) {
	cfg := config.ServerConfig{UpstreamTimeoutMS: 5000, StreamIdleTimeoutMS: 5000}
	open := NewServer(cfg, nil, nil).Handler()
	guarded := NewServer(cfg, nil, nil, WithClientKeys([]string{"sk-test"})).Handler()

	for _, tc := range []struct {
		name    string
		handler http.Handler
		body    string
		want    int
	}{
		{"missing model", open, `{}`, http.StatusBadRequest},
		{"invalid json", open, `not json`, http.StatusBadRequest},
		{"unrouted model", open, `{"model":"nope"}`, http.StatusNotFound},
		{"missing api key", guarded, `{"model":"nope"}`, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			tc.handler.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
			wantSharedEnvelope(t, rec)
		})
	}
}
