package admin

import "github.com/rajeev-chaurasia/pen-stock/internal/budget"

const (
	// unlimited is the limit value that means no cap, matching
	// budget.Limits where a zero field leaves a partially configured
	// tenant usable rather than locked out.
	unlimited = 0

	statusOK = "ok"
)

// tenantListResponse wraps the listing in an object rather than
// returning a bare array, so the schema can grow a sibling field later
// (a price version, a generated-at stamp) without breaking a parser.
type tenantListResponse struct {
	Tenants []tenantView `json:"tenants"`
}

type healthResponse struct {
	Status string `json:"status"`
}

// limitsView is the configured allowance. Zero means unlimited on every
// numeric field, exactly as budget.Limits defines it.
type limitsView struct {
	RequestsPerMinute int     `json:"requests_per_minute"`
	TokensPerMinute   int     `json:"tokens_per_minute"`
	DailyUSD          float64 `json:"daily_usd"`
	MonthlyUSD        float64 `json:"monthly_usd"`
	FailClosed        bool    `json:"fail_closed"`
}

// tenantView is one tenant's configuration and current usage. Money
// rides as JSON numbers rather than strings, so a dashboard can plot it
// without parsing.
//
// The remaining fields are pointers so that an uncapped budget renders
// as null. A literal 0 there would read as "exhausted", which is the
// exact opposite of what an absent cap means, and that misreading is
// the kind that gets acted on at 3am. Null rather than omitting the
// key: the key is then present in every response, so a consumer can
// tell "no cap" apart from "this build did not have the field".
type tenantView struct {
	Name                string     `json:"name"`
	Limits              limitsView `json:"limits"`
	DailySpentUSD       float64    `json:"daily_spent_usd"`
	MonthlySpentUSD     float64    `json:"monthly_spent_usd"`
	CommittedUSD        float64    `json:"committed_usd"`
	DailyRemainingUSD   *float64   `json:"daily_remaining_usd"`
	MonthlyRemainingUSD *float64   `json:"monthly_remaining_usd"`
}

// view projects one configured tenant into its wire form.
func (s *Server) view(name budget.TenantID) tenantView {
	limits := s.limits[name]
	daily, monthly := s.acct.Spent(name)
	committed := s.acct.Committed(name)

	return tenantView{
		Name: string(name),
		Limits: limitsView{
			RequestsPerMinute: limits.RequestsPerMinute,
			TokensPerMinute:   limits.TokensPerMinute,
			DailyUSD:          limits.DailyUSD,
			MonthlyUSD:        limits.MonthlyUSD,
			FailClosed:        limits.FailClosed,
		},
		DailySpentUSD:       daily,
		MonthlySpentUSD:     monthly,
		CommittedUSD:        committed,
		DailyRemainingUSD:   remaining(limits.DailyUSD, daily, committed),
		MonthlyRemainingUSD: remaining(limits.MonthlyUSD, monthly, committed),
	}
}

// remaining is what one more request may cost, or nil when the cap is
// unlimited.
//
// Reserved but unsettled spend counts against it because the enforcer
// counts it too: headroom that ignored reservations would show an
// operator room that the gateway will refuse to hand out. The result is
// allowed to go negative. The enforcer documents a bounded overshoot,
// and clamping at zero would hide the one number that shows it
// happened.
func remaining(limit, spent, committed float64) *float64 {
	// The enforcer applies a cap only while it is positive, so anything
	// else is uncapped here as well.
	if limit <= unlimited {
		return nil
	}
	left := limit - spent - committed
	return &left
}
