package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func call(h http.HandlerFunc, remote string) int {
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code
}

func TestLocalOnlyAllowsLoopbackOnly(t *testing.T) {
	t.Setenv("HEALTH_AGENT_LISTEN_ALL", "")
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	h := localOnly(ok)
	for remote, want := range map[string]int{
		"127.0.0.1:41234": http.StatusOK,
		"[::1]:41234":     http.StatusOK,
		"10.42.1.50:5555": http.StatusNotFound,
		"203.0.113.9:80":  http.StatusNotFound,
		"garbage":         http.StatusNotFound,
	} {
		if got := call(h, remote); got != want {
			t.Errorf("remote %q: got %d want %d", remote, got, want)
		}
	}
}

func TestLocalOnlyCanBeRelaxed(t *testing.T) {
	t.Setenv("HEALTH_AGENT_LISTEN_ALL", "true")
	h := localOnly(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	if got := call(h, "10.42.1.50:5555"); got != http.StatusOK {
		t.Errorf("HEALTH_AGENT_LISTEN_ALL=true should allow every caller, got %d", got)
	}
}

func TestPublicRoutesStayReachableFromOtherPods(t *testing.T) {
	t.Setenv("HEALTH_AGENT_LISTEN_ALL", "")
	mux := http.NewServeMux()
	(&Server{}).register(mux)
	for path, want := range map[string]int{
		"/health":           http.StatusOK,       // kubelet probe
		"/status":           http.StatusNotFound, // other pods must not see state
		"/agent":            http.StatusNotFound,
		"/api/set-pod-name": http.StatusNotFound,
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.42.1.50:5555"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("GET %s from another pod: got %d want %d", path, rec.Code, want)
		}
	}
}
