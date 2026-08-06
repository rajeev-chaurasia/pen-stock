package httperr_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/httperr"
)

// wantRateLimited is the byte for byte response a client parses. The
// literal is spelled out rather than built from the types under test,
// because a test that constructs the expectation the same way the code
// does cannot notice a renamed field, a reordered one, or a lost
// trailing newline.
const wantRateLimited = `{"error":{"message":"upstream rate limit reached","type":"rate_limit_error","code":"rate_limited"}}` + "\n"

func TestWriteErrorPinsTheWireFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	httperr.WriteError(rec, http.StatusTooManyRequests,
		"upstream rate limit reached", httperr.TypeRateLimit, "rate_limited")

	if got := rec.Body.String(); got != wantRateLimited {
		t.Errorf("body =\n%q\nwant\n%q", got, wantRateLimited)
	}
	if got := rec.Code; got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", got, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
}

// TestWriteJSONMatchesWriteError guards the other half of the contract:
// a handler that composes the envelope itself, as the streaming path
// must, still lands on the same bytes as the error writer.
func TestWriteJSONMatchesWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	httperr.WriteJSON(rec, http.StatusTooManyRequests, httperr.Envelope{Error: httperr.Body{
		Message: "upstream rate limit reached",
		Type:    httperr.TypeRateLimit,
		Code:    "rate_limited",
	}})

	if got := rec.Body.String(); got != wantRateLimited {
		t.Errorf("body =\n%q\nwant\n%q", got, wantRateLimited)
	}
}

func TestErrorTypeVocabulary(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{httperr.TypeInvalidRequest, "invalid_request_error"},
		{httperr.TypeRateLimit, "rate_limit_error"},
		{httperr.TypeAPI, "api_error"},
		{httperr.ContentTypeJSON, "application/json"},
	} {
		if tc.got != tc.want {
			t.Errorf("constant = %q, want %q", tc.got, tc.want)
		}
	}
}
