package ingress

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/rajeev-chaurasia/pen-stock/internal/budget"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// Accountant is the budgeting surface the request path uses. It is an
// interface so the ingress can be tested without a real enforcer, and
// so a gateway with no budgeting configured simply has none.
type Accountant interface {
	Begin(ctx context.Context, tenant budget.TenantID, model string, raw []byte) (*budget.Reservation, error)
	Settle(ctx context.Context, r *budget.Reservation, usage providers.Usage, model, provider string) float64
	Abort(ctx context.Context, r *budget.Reservation)
}

// writeDenial turns a refusal into the most honest answer available.
//
// The distinction that matters: a rate limit is a "come back shortly",
// and the client is told how long. An exhausted budget is not, because
// no amount of waiting refills it, so answering 429 there would send a
// well behaved client into a retry loop that can never succeed.
func (s *Server) writeDenial(w http.ResponseWriter, err error) {
	var d *budget.Denial
	if !errors.As(err, &d) {
		writeErrorJSON(w, http.StatusInternalServerError,
			"internal error", errTypeAPI, "internal")
		return
	}

	if d.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.RetryAfter.Seconds()+1)))
	}
	status, errType, code := denialToWire(d.Reason)
	writeErrorJSON(w, status, d.Message, errType, code)
}

func denialToWire(reason budget.DenyReason) (status int, errType, code string) {
	switch reason {
	case budget.DenyRequestRate:
		return http.StatusTooManyRequests, errTypeRateLimit, "tenant_request_rate_limited"
	case budget.DenyTokenRate:
		return http.StatusTooManyRequests, errTypeRateLimit, "tenant_token_rate_limited"
	case budget.DenyDailyBudget:
		// Payment required rather than too many requests: the tenant is
		// out of money for the window, and retrying cannot change that.
		return http.StatusPaymentRequired, errTypeAPI, "daily_budget_exhausted"
	case budget.DenyMonthlyBudget:
		return http.StatusPaymentRequired, errTypeAPI, "monthly_budget_exhausted"
	case budget.DenyStoreDegraded:
		// The gateway cannot tell whether there is room, and this tenant
		// asked to be stopped rather than guessed at.
		return http.StatusServiceUnavailable, errTypeAPI, "accounting_unavailable"
	case budget.DenyUnknownTenant:
		// The key authenticated but names a tenant the enforcer does not
		// know, which is a configuration fault on this side.
		return http.StatusInternalServerError, errTypeAPI, "tenant_not_configured"
	default:
		return http.StatusForbidden, errTypeAPI, "denied"
	}
}
