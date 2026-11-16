package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A server that rejects Basic Auth must be reported as an HTTP status failure, not as a body mismatch.
func TestRunHTTPPendingTestFull_BadCredentialsReportsStatusFirst(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"projectName":"gdpr","status":"UP"}]`))
	}))
	defer srv.Close()

	// With (wrong) credentials: 401, message must lead with the status and mention the credentials.
	res := RunHTTPPendingTestFull(srv.URL, 2000, `"projectName":"gdpr"`, "", "monitor", "wrong", "GET")
	if res.Passed {
		t.Fatalf("expected failure with rejected credentials")
	}
	if !strings.Contains(res.Message, "status 401") || !strings.Contains(res.Message, "Basic Auth") {
		t.Fatalf("message should report 401 and the credentials, got %q", res.Message)
	}
	if strings.Contains(res.Message, "does not contain expected text") {
		t.Fatalf("body mismatch must not mask the 401, got %q", res.Message)
	}

	// Without credentials the open endpoint passes and the body match is evaluated.
	ok := RunHTTPPendingTestFull(srv.URL, 2000, `"projectName":"gdpr"`, "", "", "", "GET")
	if !ok.Passed {
		t.Fatalf("expected pass without credentials, got %q", ok.Message)
	}
}
