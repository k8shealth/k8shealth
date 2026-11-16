package server

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

// Regression: truncated or incomplete Apache parsing must not drop Alias/DocumentRoot paths (Mounts tab).
// Needs a real httpd-vhosts.conf fixture: set HEALTH_AGENT_TEST_VHOSTS_ROOT to a directory containing it (skipped otherwise).
func TestProcessScriptPayloadHealthChecks_VhostsPaths(t *testing.T) {
	root := os.Getenv("HEALTH_AGENT_TEST_VHOSTS_ROOT")
	if root == "" {
		t.Skip("skip: HEALTH_AGENT_TEST_VHOSTS_ROOT not set")
		return
	}
	p := filepath.Join(root, "httpd-vhosts.conf")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("skip: no fixture at %s (%v)", p, err)
		return
	}

	cfg := &config.Config{PodName: "pod-0", ContainerName: "apache", Deployment: "web-site"}
	reg := registry.New(cfg)
	s := &Server{Cfg: cfg, Registry: reg}

	aliasCount := strings.Count(string(raw), "\nAlias ")
	if aliasCount < 10 {
		t.Fatalf("fixture sanity: expected many Alias lines, got %d", aliasCount)
	}

	payload := models.ScriptPayload{
		Pod:       "pod-0",
		Container: "apache",
		Files: []models.ConfigFile{{
			Path:   "/usr/local/apache2/conf/extra/httpd-vhosts.conf",
			Type:   "file",
			Base64: base64.StdEncoding.EncodeToString(raw),
		}},
		FileAccessTargets: nil,
		Timestamp:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	s.ProcessScriptPayloadHealthChecks(&payload)

	targets := reg.GetFileAccessTargets("pod-0", "apache")
	if len(targets) < aliasCount {
		t.Fatalf("expected at least %d file access targets from Alias lines, got %d", aliasCount, len(targets))
	}

	seen := make(map[string]bool)
	for _, tg := range targets {
		seen[tg.Path] = true
	}
	for _, must := range []string{
		"/static/fx_static",
		"/usr/local/tomcat/images",
		"/usr/local/tomcat",
	} {
		if !seen[must] {
			t.Errorf("missing expected mount path %q in targets", must)
		}
	}
}
