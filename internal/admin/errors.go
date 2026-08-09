package admin

import (
	"net/http"

	"github.com/rajeev-chaurasia/pen-stock/internal/httperr"
)

const (
	contentTypeJSON = httperr.ContentTypeJSON

	// errTypeInvalidRequest comes from the shared envelope's vocabulary,
	// so an operator's tooling can parse this surface and the proxy one
	// the same way. It is the only type this API can produce: every
	// failure here is a request for something that is not there.
	errTypeInvalidRequest = httperr.TypeInvalidRequest

	codeTenantNotFound   = "tenant_not_found"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	httperr.WriteJSON(w, status, v)
}

// writeError answers with the gateway's JSON error envelope. Every path
// on this handler goes through it, so no caller ever meets net/http's
// plain text default.
func writeError(w http.ResponseWriter, status int, message, code string) {
	httperr.WriteError(w, status, message, errTypeInvalidRequest, code)
}
