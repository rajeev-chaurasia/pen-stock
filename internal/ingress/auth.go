package ingress

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

const bearerPrefix = "Bearer "

// keySet holds client API keys as digests so a comparison never runs
// against the raw secret and never short circuits on length.
type keySet struct {
	digests [][sha256.Size]byte
}

func newKeySet(keys []string) *keySet {
	set := &keySet{digests: make([][sha256.Size]byte, 0, len(keys))}
	for _, key := range keys {
		if key == "" {
			continue
		}
		set.digests = append(set.digests, sha256.Sum256([]byte(key)))
	}
	return set
}

func (k *keySet) empty() bool { return len(k.digests) == 0 }

// allows reports whether presented matches any configured key. Every
// candidate is compared so timing does not reveal which key matched.
func (k *keySet) allows(presented string) bool {
	got := sha256.Sum256([]byte(presented))
	var matched int
	for i := range k.digests {
		matched |= subtle.ConstantTimeCompare(got[:], k.digests[i][:])
	}
	return matched == 1
}

// withAuth rejects requests that do not present a configured client key.
// Health checks stay open so a load balancer needs no credential.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.clientKeys.empty() || r.URL.Path == healthPath {
			next.ServeHTTP(w, r)
			return
		}
		key, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || !s.clientKeys.allows(key) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErrorJSON(w, http.StatusUnauthorized,
				"a valid client API key is required", errTypeInvalidRequest, "invalid_api_key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(header string) (string, bool) {
	if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(bearerPrefix):]), true
}
