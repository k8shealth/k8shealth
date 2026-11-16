package health

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fx/health-agent-go/registry"
)

func errPageServer(code int, server, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if server != "" {
			w.Header().Set("Server", server)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
}

// A failing HTTP check must name the layer that produced the error page, whatever the check configuration.
func TestHTTPCheckNamesTheRespondingLayerOnError(t *testing.T) {
	page := "<html><body><center><h1>403 Forbidden</h1></center><hr><center>nginx</center></body></html>"
	nginx403 := errPageServer(403, "", page)
	defer nginx403.Close()
	nginxBody := errPageServer(403, "", page)
	defer nginxBody.Close()
	nginxHead := errPageServer(403, "", page)
	defer nginxHead.Close()
	ok := errPageServer(200, "nginx", "fine")
	defer ok.Close()
	haproxy := errPageServer(403, "", "<h1>403 Forbidden</h1>\nRequest forbidden by administrative rules.")
	defer haproxy.Close()

	reg := registry.New(nil)
	runHTTPCheck("pod", "c", ResolvedCheck{Name: "plain", Kind: "http", URL: nginx403.URL}, reg)
	runHTTPCheck("pod", "c", ResolvedCheck{Name: "withBodyMatch", Kind: "http", URL: nginxBody.URL, Contains: "UP"}, reg)
	runHTTPCheck("pod", "c", ResolvedCheck{Name: "head", Kind: "http", URL: nginxHead.URL, Method: "HEAD"}, reg)
	runHTTPCheck("pod", "c", ResolvedCheck{Name: "passing", Kind: "http", URL: ok.URL}, reg)
	runHTTPCheck("pod", "c", ResolvedCheck{Name: "lb", Kind: "http", URL: haproxy.URL}, reg)

	got := map[string]string{}
	for _, h := range reg.GetHealthBaseChecks()["pod"]["c"] {
		got[h.Name] = h.Responder
	}
	if got["plain"] != "nginx" || got["withBodyMatch"] != "nginx" {
		t.Fatalf("expected nginx, got %v", got)
	}
	if got["lb"] != "HAProxy" {
		t.Fatalf("expected HAProxy, got %v", got)
	}
	if got["passing"] != "" {
		t.Fatalf("a passing check must not report a responder: %v", got)
	}
	// HEAD has no body, so only the Server header could identify it; the stock page has none -> unknown, not a guess.
	if got["head"] != "" {
		t.Fatalf("HEAD error without identifying headers must stay unnamed, got %q", got["head"])
	}
}

func TestScriptHTTPCheckNamesTheRespondingLayer(t *testing.T) {
	srv := errPageServer(502, "", "<center>nginx</center>")
	defer srv.Close()
	h, ok := BuildScriptHTTPCheck(srv.URL)
	if !ok || h.Passed || h.Responder != "nginx" {
		t.Fatalf("got ok=%v passed=%v responder=%q", ok, h.Passed, h.Responder)
	}
}

