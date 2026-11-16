package main

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

// secretTransport adds the optional shared secret (X-Agent-Secret) to every request sent to the manager.
// All agent HTTP clients use http.DefaultTransport, so wrapping it once covers callback, sync, pending
// tests, health-check configs, etc. Requests to any other host (health-check targets, peers) never get it.
// Backward compatible: without HEALTH_AGENT_SECRET nothing is added.
type secretTransport struct {
	base        http.RoundTripper
	secret      string
	managerHost string
}

func (t *secretTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.secret != "" && strings.EqualFold(req.URL.Host, t.managerHost) {
		r2 := req.Clone(req.Context())
		r2.Header.Set("X-Agent-Secret", t.secret)
		return t.base.RoundTrip(r2)
	}
	return t.base.RoundTrip(req)
}

// installAgentSecret wraps http.DefaultTransport when HEALTH_AGENT_SECRET is set. Returns true if installed.
func installAgentSecret(managerURL string) bool {
	secret := strings.TrimSpace(os.Getenv("HEALTH_AGENT_SECRET"))
	if secret == "" {
		return false
	}
	u, err := url.Parse(managerURL)
	if err != nil || u.Host == "" {
		return false
	}
	http.DefaultTransport = &secretTransport{base: http.DefaultTransport, secret: secret, managerHost: u.Host}
	return true
}
