package server

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

func TestProcessScriptPayloadHealthChecks_SkipsCommentedApacheDirectives(t *testing.T) {
	cfg := &config.Config{PodName: "pod-1", ContainerName: "apache", Deployment: "web-apache"}
	reg := registry.New(cfg)
	s := &Server{Cfg: cfg, Registry: reg}

	conf := "" +
		"#<Directory \"/commented/out\">\n" +
		"  # Alias /x \"/also/commented\"\n" +
		"# ProxyPass /bad http://127.0.0.1:9999/\n" +
		"ProxyPass /good http://127.0.0.1:8080/\n" +
		"<Directory \"/var/www/html\">\n" +
		"  AllowOverride All\n" +
		"</Directory>\n"

	payload := models.ScriptPayload{
		Pod:       "pod-1",
		Container: "apache",
		Files: []models.ConfigFile{{
			Path:   "/etc/apache2/sites-enabled/000-default.conf",
			Type:   "file",
			Base64: base64.StdEncoding.EncodeToString([]byte(conf)),
		}},
		FileAccessTargets: nil,
		Timestamp:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	s.ProcessScriptPayloadHealthChecks(&payload)

	targets := reg.GetFileAccessTargets("pod-1", "apache")
	seen := make(map[string]bool)
	for _, tg := range targets {
		seen[tg.Path] = true
	}
	if seen["/commented/out"] || seen["/also/commented"] {
		t.Fatalf("commented-out paths must not become file targets: %#v", seen)
	}
	if !seen["/var/www/html"] {
		t.Fatalf("expected active <Directory> path /var/www/html in targets, got %#v", seen)
	}

	var urls []string
	for _, c := range reg.GetHealthBaseChecks()["pod-1"]["apache"] {
		if c.Type == "http" && c.URL != "" {
			urls = append(urls, c.URL)
		}
	}
	for _, u := range urls {
		if u == "http://127.0.0.1:9999/" {
			t.Fatalf("commented ProxyPass URL must not create HTTP check, got %v", urls)
		}
	}
	foundGood := false
	for _, u := range urls {
		if u == "http://127.0.0.1:8080/" {
			foundGood = true
		}
	}
	if !foundGood {
		t.Fatalf("expected active ProxyPass URL in HTTP checks, got %v", urls)
	}
}

func TestProcessScriptPayloadHealthChecks_SkipsCommentedHaproxyLines(t *testing.T) {
	cfg := &config.Config{PodName: "pod-h", ContainerName: "haproxy", Deployment: "cluster-haproxy"}
	reg := registry.New(cfg)
	s := &Server{Cfg: cfg, Registry: reg}

	cfgText := "" +
		"backend b1\n" +
		"  option httpchk GET /health\n" +
		"  # server s1 10.0.0.1:80 check\n" +
		"  ; server s2 10.0.0.2:80 check\n" +
		"  server s3 10.0.0.3:80 check\n"

	payload := models.ScriptPayload{
		Pod:       "pod-h",
		Container: "haproxy",
		Files: []models.ConfigFile{{
			Path:   "/etc/haproxy/haproxy.cfg",
			Type:   "haproxy",
			Base64: base64.StdEncoding.EncodeToString([]byte(cfgText)),
		}},
		FileAccessTargets: nil,
		Timestamp:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	s.ProcessScriptPayloadHealthChecks(&payload)

	var urls []string
	for _, c := range reg.GetHealthBaseChecks()["pod-h"]["haproxy"] {
		if c.Type == "http" && c.URL != "" {
			urls = append(urls, c.URL)
		}
	}
	for _, u := range urls {
		if u == "http://10.0.0.1:80/health" || u == "http://10.0.0.2:80/health" {
			t.Fatalf("commented HAProxy server lines must not create checks, got %v", urls)
		}
	}
	ok := false
	for _, u := range urls {
		if u == "http://10.0.0.3:80/health" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("expected server s3 URL in checks, got %v", urls)
	}
}
