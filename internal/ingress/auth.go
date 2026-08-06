package ingress

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

const bearerPrefix = "Bearer "

// AnonymousTenant is the identity of a caller who authenticated with a
// key that belongs to no tenant. Spend still gets recorded, it just has
// nobody to bill.
const AnonymousTenant = ""

// keySet holds client API keys as digests so a comparison never runs
// against the raw secret and never short circuits on length. Each digest
// carries the tenant that owns it, which is how an authenticated request
// acquires an identity to bill.
type keySet struct {
	digests [][sha256.Size]byte
	tenants []string
}

func newKeySet(keys []string) *keySet {
	set := &keySet{}
	for _, key := range keys {
		set.add(key, AnonymousTenant)
	}
	return set
}

func (k *keySet) add(key, tenant string) {
	if key == "" {
		return
	}
	k.digests = append(k.digests, sha256.Sum256([]byte(key)))
	k.tenants = append(k.tenants, tenant)
}

func (k *keySet) empty() bool { return len(k.digests) == 0 }

// lookup reports whether presented matches a configured key and which
// tenant owns it. Every candidate is compared, so timing reveals neither
// whether a key matched nor which one.
func (k *keySet) lookup(presented string) (tenant string, ok bool) {
	got := sha256.Sum256([]byte(presented))
	matchedAt := -1
	var matched int
	for i := range k.digests {
		if subtle.ConstantTimeCompare(got[:], k.digests[i][:]) == 1 {
			matched = 1
			matchedAt = i
		}
	}
	if matched != 1 {
		return AnonymousTenant, false
	}
	return k.tenants[matchedAt], true
}

// withAuth rejects requests that do not present a configured key, and
// tags the ones it admits with the tenant that owns the key.
// Health checks stay open so a load balancer needs no credential.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.clientKeys.empty() || r.URL.Path == healthPath {
			next.ServeHTTP(w, r)
			return
		}
		key, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			s.rejectUnauthorized(w)
			return
		}
		tenant, ok := s.clientKeys.lookup(key)
		if !ok {
			s.rejectUnauthorized(w)
			return
		}
		if tenant != AnonymousTenant {
			logInfoFrom(r.Context()).tenant = tenant
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rejectUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeErrorJSON(w, http.StatusUnauthorized,
		"a valid client API key is required", errTypeInvalidRequest, "invalid_api_key")
}

func bearerToken(header string) (string, bool) {
	if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(bearerPrefix):]), true
}
