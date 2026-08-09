package providers

import "net/http"

// DefaultMaxIdleConnsPerHost widens the stdlib's per-host idle pool. A
// gateway fans many concurrent requests into a handful of upstream
// hosts, which is the opposite of the browser-shaped traffic the default
// of 2 is tuned for: without this, connections are torn down and
// redialled under exactly the load the pool exists to serve.
const DefaultMaxIdleConnsPerHost = 32

// NewHTTPClient returns the client every outbound caller in this
// repository uses. It keeps the stdlib transport defaults, which is what
// carries proxy support, TLS configuration and HTTP/2, and changes only
// the idle pool.
//
// There is deliberately no client Timeout. Deadlines arrive on the
// context, where a caller can set one that reflects its own budget, and
// a client timeout would cut a long stream off mid answer no matter how
// healthy it was.
func NewHTTPClient() *http.Client {
	transport := &http.Transport{}
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = t.Clone()
	}
	transport.MaxIdleConnsPerHost = DefaultMaxIdleConnsPerHost
	return &http.Client{Transport: transport}
}

// Is2xx reports whether an upstream status means success. Every adapter
// needs this and none of them need their own.
func Is2xx(code int) bool { return code >= 200 && code < 300 }
