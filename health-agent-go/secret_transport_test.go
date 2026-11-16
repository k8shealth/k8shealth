package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecretOnlySentToManager(t *testing.T) {
	var gotMgr, gotOther string
	mgr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotMgr = r.Header.Get("X-Agent-Secret") }))
	defer mgr.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotOther = r.Header.Get("X-Agent-Secret") }))
	defer other.Close()

	orig := http.DefaultTransport
	defer func() { http.DefaultTransport = orig }()
	t.Setenv("HEALTH_AGENT_SECRET", "s3cret")
	if !installAgentSecret(mgr.URL) {
		t.Fatal("expected transport installed")
	}
	if _, err := http.Get(mgr.URL + "/api/sync"); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(other.URL + "/x"); err != nil {
		t.Fatal(err)
	}
	if gotMgr != "s3cret" {
		t.Fatalf("manager did not get secret: %q", gotMgr)
	}
	if gotOther != "" {
		t.Fatalf("secret leaked to other host: %q", gotOther)
	}
}

func TestNoSecretNoTransport(t *testing.T) {
	t.Setenv("HEALTH_AGENT_SECRET", "")
	if installAgentSecret("http://x:8080") {
		t.Fatal("should not install without secret")
	}
}
