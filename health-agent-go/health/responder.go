package health

import (
	"net/http"
	"regexp"
	"strings"
)

// ClassifyResponder names the layer that produced an HTTP error response ("nginx", "HAProxy", "Cloudflare", ...),
// so an incident can say "HTTP 403 - nginx" instead of a bare "HTTP 403" and the on-call goes straight to the right
// hop of the chain (CDN -> load balancer -> ingress -> app). It is a best-effort label: it returns "" when the
// signals are not recognised. Error-page bodies are checked first because a proxy such as Cloudflare keeps its own
// Server header even when it is relaying the origin's error page.
//
// NOTE: health-probe/probe/responder.go carries the same logic for external monitors - keep them in sync.
func ClassifyResponder(h http.Header, body string) string {
	lb := strings.ToLower(body)
	server := strings.TrimSpace(h.Get("Server"))
	lserver := strings.ToLower(server)

	// 1. Cloudflare's own error/challenge page.
	if strings.Contains(lb, "cloudflare") &&
		(strings.Contains(lb, "ray id") || strings.Contains(lb, "attention required") ||
			strings.Contains(lb, "cf-error") || strings.Contains(lb, "checking your browser")) {
		return "Cloudflare"
	}
	if h.Get("cf-mitigated") != "" {
		return "Cloudflare"
	}
	// 2. HAProxy stock pages (it sends no Server header).
	if strings.Contains(lb, "request forbidden by administrative rules") ||
		strings.Contains(lb, "no server is available to handle this request") ||
		strings.Contains(lb, "haproxy") {
		return "HAProxy"
	}
	// 3. Envoy / Istio.
	if strings.Contains(lb, "upstream connect error") || strings.Contains(lb, "no healthy upstream") ||
		strings.Contains(lb, "rbac: access denied") || lserver == "envoy" || lserver == "istio-envoy" {
		return "Envoy"
	}
	// 4. nginx family (stock pages carry <center>nginx</center>; ingress-nginx default backend says so explicitly).
	if strings.Contains(lb, "default backend - 404") {
		return "nginx ingress (default backend)"
	}
	if nginxPage.MatchString(lb) || strings.HasPrefix(lserver, "nginx") {
		return "nginx"
	}
	if strings.Contains(lb, "openresty") || strings.HasPrefix(lserver, "openresty") {
		return "openresty"
	}
	if strings.Contains(lb, "apache") && strings.Contains(lb, "server at") || strings.HasPrefix(lserver, "apache") {
		return "Apache"
	}
	if strings.HasPrefix(lserver, "traefik") {
		return "Traefik"
	}
	// 5. Whatever the response says it is, if it is not just Cloudflare relaying something we could not read.
	if lserver != "" && !strings.Contains(lserver, "cloudflare") {
		return sanitizeResponder(server)
	}
	if h.Get("CF-RAY") != "" || strings.Contains(lserver, "cloudflare") {
		return "Cloudflare"
	}
	if via := strings.TrimSpace(h.Get("Via")); via != "" {
		return sanitizeResponder("via " + via)
	}
	return ""
}

var nginxPage = regexp.MustCompile(`<center>\s*nginx[^<]*</center>`)

// sanitizeResponder keeps only printable ASCII and caps the length: the value ends up in incident text and
// notifications, so a hostile Server header must not be able to inject markup or newlines.
func sanitizeResponder(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f && r != '<' && r != '>' && r != '"' && r != '`' {
			b.WriteRune(r)
		}
		if b.Len() >= 40 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
