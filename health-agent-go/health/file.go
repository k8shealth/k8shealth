package health

import (
	"fmt"
	"strings"
	"time"

	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

// runFileCheck evaluates a file/directory check defined in the manager UI/DB or YAML.
//
// The sidecar has its own filesystem: the application's paths (/var/www/..., mounted volumes) do not exist
// here, so stat-ing them from the agent always failed with "no such file or directory". The path is instead
// handed to the script that runs inside the application container (via the file access targets returned by
// /api/script-config) and the check is judged from that script's readable/writable measurement.
//
// Until the script has reported the path the check stays pending (nothing is recorded and it counts as not
// failed) - a missing result must not raise an incident.
func runFileCheck(pod, container string, c ResolvedCheck, reg *registry.Registry) bool {
	path := strings.TrimSpace(c.Path)
	if path == "" {
		// No path configured - treat as pass so it doesn't show as broken
		reg.AddHealthCheck(pod, container, "file", models.HealthCheck{
			Type: "file", Name: c.Name, Passed: true, Time: time.Now().UTC(), Mode: c.Mode,
			Message: "no path configured",
		})
		return true
	}
	mode := models.NormalizeFileMode(c.Mode)
	reg.AddFileAccessTarget(pod, container, models.FileAccessTarget{Path: path, Mode: mode})

	t, ok := reg.GetScriptFileTarget(pod, container, path)
	if !ok {
		return true // pending: the script has not measured this path yet
	}
	passed := models.FileAccessPassed(mode, t.Readable, t.Writable)
	duration := t.ReadDurationMs
	if duration <= 0 && t.WriteDurationMs > 0 {
		duration = t.WriteDurationMs
	}
	reg.AddHealthCheck(pod, container, "file", models.HealthCheck{
		Type:     "file",
		Name:     c.Name,
		Passed:   passed,
		Duration: duration,
		Time:     time.Now().UTC(),
		Path:     path,
		Mode:     c.Mode,
		Message:  fileCheckMessage(mode, passed, t),
	})
	return passed
}

// fileCheckMessage is the human text shown as the check's response.
func fileCheckMessage(mode string, passed bool, t models.FileAccessTarget) string {
	if passed {
		return "File check passed"
	}
	if t.TimedOut {
		d := t.ReadDurationMs
		if t.WriteDurationMs > d {
			d = t.WriteDurationMs
		}
		if d > 0 {
			return fmt.Sprintf("timed out after %dms", d)
		}
		return "timed out"
	}
	if !t.Readable {
		return "not readable"
	}
	if mode == "rw" {
		return "not writable"
	}
	return "unexpected write access (expected read-only)"
}
