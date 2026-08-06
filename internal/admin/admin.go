// Package admin serves Penstock's operator API: what each tenant is
// allowed to spend, and what it has spent so far.
//
// The surface is deliberately narrow. A response carries tenant names,
// configured limits, and money. It never carries a client key, a
// provider key, a prompt, or a completion, and this package is not
// given access to any of them in the first place. The listener this
// mounts on defaults to loopback, but that is a second line of defense:
// nothing here would become unsafe if the listener were exposed.
//
// The schema has no price_version field. Pricing is not a dependency of
// this package, so the wiring layer is the only place that can supply
// one.
package admin

import (
	"net/http"
	"slices"

	"github.com/rajeev-chaurasia/pen-stock/internal/budget"
)

const (
	pathTenants = "/admin/tenants"
	pathTenant  = "/admin/tenants/{name}"
	pathHealthz = "/healthz"

	// allowedMethods is what the Allow header advertises on a method
	// mismatch. HEAD needs no mention: a server that answers GET must
	// answer HEAD, and the mux already routes it that way.
	allowedMethods = http.MethodGet
)

// Accounting is the slice of the budget enforcer the admin API reads.
type Accounting interface {
	Spent(t budget.TenantID) (daily, monthly float64)
	Committed(t budget.TenantID) float64
}

// Server answers operator queries about tenant budgets.
//
// Its own fields are fixed at construction, so the only concurrency it
// has to think about lives behind Accounting, which the enforcer
// already serializes.
type Server struct {
	acct   Accounting
	limits map[budget.TenantID]budget.Limits
	// names is sorted so the listing is stable across calls and two
	// dumps of it can be diffed.
	names   []budget.TenantID
	handler http.Handler
}

// New builds the admin API over a fixed set of configured tenants.
// limits is copied, so a later edit by the caller cannot quietly change
// what operators are told is configured.
func New(acct Accounting, limits map[budget.TenantID]budget.Limits) *Server {
	if acct == nil {
		// Budgeting is optional in this gateway. Without it the endpoint
		// still reports configuration rather than panicking per request.
		acct = zeroAccounting{}
	}

	s := &Server{
		acct:   acct,
		limits: make(map[budget.TenantID]budget.Limits, len(limits)),
		names:  make([]budget.TenantID, 0, len(limits)),
	}
	for id, l := range limits {
		s.limits[id] = l
		s.names = append(s.names, id)
	}
	slices.Sort(s.names)

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pathTenants, s.handleTenantList)
	mux.HandleFunc("GET "+pathTenant, s.handleTenant)
	mux.HandleFunc("GET "+pathHealthz, handleHealthz)

	// The mux answers a method mismatch by itself, in plain text. These
	// method-less twins match strictly more requests than the GET
	// patterns above, so they pick up only the mismatches, and they
	// answer in JSON like everything else on this API.
	mux.HandleFunc(pathTenants, handleMethodNotAllowed)
	mux.HandleFunc(pathTenant, handleMethodNotAllowed)
	mux.HandleFunc(pathHealthz, handleMethodNotAllowed)

	mux.HandleFunc("/", handleNotFound)

	s.handler = mux
	return s
}

// Handler returns the wired handler, ready to mount on the admin
// listener.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) handleTenantList(w http.ResponseWriter, _ *http.Request) {
	list := make([]tenantView, 0, len(s.names))
	for _, name := range s.names {
		list = append(list, s.view(name))
	}
	writeJSON(w, http.StatusOK, tenantListResponse{Tenants: list})
}

func (s *Server) handleTenant(w http.ResponseWriter, r *http.Request) {
	name := budget.TenantID(r.PathValue("name"))
	if _, configured := s.limits[name]; !configured {
		// The requested name is not echoed back. It is caller controlled
		// text, and repeating it buys the operator nothing they did not
		// already type.
		writeError(w, http.StatusNotFound, "tenant is not configured", codeTenantNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.view(name))
}

// handleHealthz reports that the admin listener itself is up, so it can
// be probed independently of the public one.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: statusOK})
}

func handleMethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", allowedMethods)
	writeError(w, http.StatusMethodNotAllowed, "this endpoint is read only", codeMethodNotAllowed)
}

func handleNotFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "unknown admin endpoint", codeNotFound)
}

// zeroAccounting stands in when no enforcer was wired. Zeros read as
// "nothing spent", which is the truth when nothing is metering spend.
type zeroAccounting struct{}

func (zeroAccounting) Spent(budget.TenantID) (daily, monthly float64) { return 0, 0 }

func (zeroAccounting) Committed(budget.TenantID) float64 { return 0 }
