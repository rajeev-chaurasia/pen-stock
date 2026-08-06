package ingress

import (
	"net/http"
	"strconv"
)

// retryAfterSaturated is advertised when the gateway is at capacity.
const retryAfterSaturated = 1

// inflight bounds concurrent requests. Each in flight request can hold a
// request body plus an upstream response, so an unbounded count is an
// unbounded memory ceiling.
type inflight struct {
	slots chan struct{}
}

func newInflight(limit int) *inflight {
	if limit <= 0 {
		return &inflight{}
	}
	return &inflight{slots: make(chan struct{}, limit)}
}

// withLimit sheds load rather than queueing it: a caller that cannot get
// a slot immediately is told to retry instead of waiting behind others.
func (s *Server) withLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.inflight.slots == nil {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case s.inflight.slots <- struct{}{}:
			defer func() { <-s.inflight.slots }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSaturated))
			writeErrorJSON(w, http.StatusServiceUnavailable,
				"gateway is at capacity, retry shortly", errTypeAPI, "gateway_saturated")
		}
	})
}
