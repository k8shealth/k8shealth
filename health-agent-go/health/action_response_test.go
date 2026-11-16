package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestActionResponseOK(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{200, `{"success":true}`, true},
		{200, ``, true},
		{200, `<html>login</html>`, true},
		{200, `{"success":false,"message":"Use POST"}`, false},
		{405, `{"success":false,"message":"Use POST to create a thread dump."}`, false},
		{302, ``, false},
		{500, `boom`, false},
	}
	for _, c := range cases {
		got, _ := actionResponseOK(c.status, []byte(c.body))
		if got != c.want {
			t.Errorf("status=%d body=%q: got %v want %v", c.status, c.body, got, c.want)
		}
	}
}

// A POST redirected to a 200 page must be reported as a failure (redirect not followed).
func TestRunActionPendingTestRedirectIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.WriteHeader(200)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer srv.Close()
	res := RunActionPendingTest(srv.URL+"/trigger", "POST", "")
	if res.Passed {
		t.Fatalf("redirect to login page reported as success: %s", res.Message)
	}
	if !strings.Contains(res.Message, "redirect") {
		t.Errorf("message should mention redirect, got %q", res.Message)
	}
}
