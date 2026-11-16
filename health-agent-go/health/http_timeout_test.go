package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

func slowServer(d time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(d)
		_, _ = w.Write([]byte(`{"status":"UP"}`))
	}))
}

func loadOne(t *testing.T, value string) ResolvedCheck {
	t.Helper()
	cs, err := LoadFromEnv(map[string]string{
		"HC_CHECKS_JSON": `[{"name":"dep","kind":"http","value":` + value + `}]`,
	})
	if err != nil || len(cs) != 1 {
		t.Fatalf("load failed: %v %v", cs, err)
	}
	return cs[0]
}

// YAML timeoutMs must be honoured and legacy "result":"UP" must act as a body match.
func TestHTTPCheckHonoursYamlTimeoutAndLegacyResult(t *testing.T) {
	srv := slowServer(1500 * time.Millisecond)
	defer srv.Close()

	c := loadOne(t, `{"url":"`+srv.URL+`","result":"UP","timeoutMs":5000}`)
	if c.TimeoutMs != 5000 || c.Contains != "UP" {
		t.Fatalf("parsed wrong: timeout=%d contains=%q", c.TimeoutMs, c.Contains)
	}
	reg := registry.New(nil)
	if !runHTTPCheck("pod", "c", c, reg) {
		t.Fatal("expected pass with generous timeout")
	}

	// Too-short timeout must fail (proves the value is actually applied).
	c.TimeoutMs = 300
	if runHTTPCheck("pod", "c", c, reg) {
		t.Fatal("expected failure with 300ms timeout")
	}
}

// The reported HealthCheck must carry the effective timeout/method; defaults when unset.
func TestHTTPCheckReportsEffectiveTimeoutAndMethod(t *testing.T) {
	srv := slowServer(0)
	defer srv.Close()
	srv2 := slowServer(0)
	defer srv2.Close()
	reg := registry.New(nil)

	runHTTPCheck("pod", "c", ResolvedCheck{Name: "a", Kind: "http", URL: srv.URL, TimeoutMs: 60000, Method: "head"}, reg)
	runHTTPCheck("pod", "c", ResolvedCheck{Name: "b", Kind: "http", URL: srv2.URL}, reg)

	got := map[string]models.HealthCheck{}
	for _, h := range reg.GetHealthBaseChecks()["pod"]["c"] {
		got[h.Name] = h
	}
	if got["a"].TimeoutMs != 60000 || got["a"].HTTPMethod != "HEAD" {
		t.Fatalf("a: timeout=%d method=%q", got["a"].TimeoutMs, got["a"].HTTPMethod)
	}
	if got["b"].TimeoutMs != 5000 || got["b"].HTTPMethod != "GET" {
		t.Fatalf("b: timeout=%d method=%q", got["b"].TimeoutMs, got["b"].HTTPMethod)
	}
}

// Backward compatibility of the wire format: zero values are omitted, so a payload
// without the new fields is byte-identical to what older agents sent.
func TestHealthCheckJSONOmitsNewFieldsWhenEmpty(t *testing.T) {
	b, _ := json.Marshal(models.HealthCheck{Type: "mysql", Name: "m"})
	s := string(b)
	if strings.Contains(s, "timeoutMs") || strings.Contains(s, "httpMethod") {
		t.Fatalf("new fields leaked into payload: %s", s)
	}
	b, _ = json.Marshal(models.HealthCheck{Type: "http", Name: "h", TimeoutMs: 1, HTTPMethod: "GET"})
	if !strings.Contains(string(b), `"timeoutMs":1`) || !strings.Contains(string(b), `"httpMethod":"GET"`) {
		t.Fatalf("missing fields: %s", b)
	}
}


