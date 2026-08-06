package providers_test

import (
	"net/http"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

func TestClassFromStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   providers.ErrorClass
	}{
		{"unauthorized", http.StatusUnauthorized, providers.ErrClassAuth},
		{"forbidden", http.StatusForbidden, providers.ErrClassAuth},
		// Seen live from Cerebras when a tier was never activated. It is
		// neither an auth failure nor a rate limit: the key is valid and
		// waiting will not help, so it needs its own bucket.
		{"payment required", http.StatusPaymentRequired, providers.ErrClassPaymentRequired},
		{"too many requests", http.StatusTooManyRequests, providers.ErrClassRateLimited},
		{"bad request", http.StatusBadRequest, providers.ErrClassInvalidRequest},
		{"unprocessable", http.StatusUnprocessableEntity, providers.ErrClassInvalidRequest},
		{"server error", http.StatusInternalServerError, providers.ErrClassUpstream},
		{"overloaded", 529, providers.ErrClassUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providers.ClassFromStatus(tc.status); got != tc.want {
				t.Errorf("ClassFromStatus(%d) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestClassFromStatusAndBodyDisambiguates404(t *testing.T) {
	cases := []struct {
		name string
		body string
		want providers.ErrorClass
	}{
		{
			name: "provider error envelope means the model is missing",
			body: `{"error":{"message":"model does not exist"}}`,
			want: providers.ErrClassModelNotFound,
		},
		{
			// A router's plain text 404 means the base_url is wrong,
			// which is gateway misconfiguration, not a missing model.
			name: "plain text means a mistyped base_url",
			body: "404 page not found",
			want: providers.ErrClassUpstream,
		},
		{
			name: "html error page means a mistyped base_url",
			body: "<html><body>Not Found</body></html>",
			want: providers.ErrClassUpstream,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providers.ClassFromStatusAndBody(http.StatusNotFound, []byte(tc.body))
			if got != tc.want {
				t.Errorf("class = %q, want %q", got, tc.want)
			}
		})
	}
}
