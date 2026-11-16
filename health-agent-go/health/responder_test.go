package health

import (
	"net/http"
	"testing"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestClassifyResponder(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		body string
		want string
	}{
		{"nginx stock 403 page", hdr(), "<html><head><title>403 Forbidden</title></head><body><center><h1>403 Forbidden</h1></center><hr><center>nginx</center></body></html>", "nginx"},
		{"nginx with version in body", hdr(), "<hr><center>nginx/1.25.3</center>", "nginx"},
		{"nginx via Server header only", hdr("Server", "nginx"), "", "nginx"},
		{"ingress default backend", hdr(), "default backend - 404", "nginx ingress (default backend)"},
		{"haproxy admin rules", hdr(), "<html><body><h1>403 Forbidden</h1>\nRequest forbidden by administrative rules.\n</body></html>", "HAProxy"},
		{"haproxy no server", hdr(), "<h1>503 Service Unavailable</h1>\nNo server is available to handle this request.", "HAProxy"},
		{"cloudflare error page", hdr("Server", "cloudflare", "CF-RAY", "abc"), "<title>Attention Required! | Cloudflare</title> Cloudflare Ray ID: abc", "Cloudflare"},
		{"cloudflare relaying origin nginx page", hdr("Server", "cloudflare", "CF-RAY", "abc"), "<center>nginx</center>", "nginx"},
		{"cloudflare with unreadable body", hdr("Server", "cloudflare", "CF-RAY", "abc"), "", "Cloudflare"},
		{"envoy", hdr(), "upstream connect error or disconnect/reset before headers", "Envoy"},
		{"apache", hdr("Server", "Apache/2.4.58 (Ubuntu)"), "", "Apache"},
		{"unknown server header is passed through", hdr("Server", "gunicorn"), "", "gunicorn"},
		{"via only", hdr("Via", "1.1 varnish"), "", "via 1.1 varnish"},
		{"nothing recognisable", hdr(), "oops", ""},
	}
	for _, c := range cases {
		if got := ClassifyResponder(c.h, c.body); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestSanitizeResponderStripsMarkupAndCapsLength(t *testing.T) {
	got := sanitizeResponder("evil<script>\nalert(1)</script>" + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	for _, r := range got {
		if r == '<' || r == '>' || r == '\n' {
			t.Fatalf("unsafe character in %q", got)
		}
	}
	if len(got) > 40 {
		t.Fatalf("too long: %d", len(got))
	}
}
