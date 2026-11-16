package health

import (
	"testing"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

func fileResults(reg *registry.Registry) []models.HealthCheck {
	var out []models.HealthCheck
	for _, byContainer := range reg.GetHealthBaseChecks() {
		for _, hs := range byContainer {
			for _, h := range hs {
				if h.Type == "file" {
					out = append(out, h)
				}
			}
		}
	}
	return out
}

func newFileTestRegistry() *registry.Registry {
	return registry.New(&config.Config{PodName: "pod-1", Deployment: "dep", ContainerName: "app"})
}

// The path does not exist in the agent's own filesystem; the script (inside the app container) saw it fine.
func TestFileCheckUsesScriptResultNotSidecarFilesystem(t *testing.T) {
	reg := newFileTestRegistry()
	c := ResolvedCheck{Name: "blog.conf", Kind: "file", Path: "/var/www/html/wordpress", Mode: "read"}

	// Nothing measured yet: stays pending, no failure recorded.
	if !runFileCheck("pod-1", "app", c, reg) {
		t.Fatal("pending check must not count as failed")
	}
	if got := fileResults(reg); len(got) != 0 {
		t.Fatalf("pending check recorded a result: %+v", got)
	}
	// ...but the path is handed to the script.
	found := false
	for _, ft := range reg.GetFileAccessTargets("pod-1", "app") {
		if ft.Path == c.Path && ft.Mode == "ro" {
			found = true
		}
	}
	if !found {
		t.Fatal("path was not handed to the script as a ro target")
	}

	// Script reports: readable, not writable -> "read" (read-only) passes.
	reg.AddScriptPayload("pod-1", models.ScriptPayload{Pod: "pod-1", Container: "app", FileAccessTargets: []models.FileAccessTarget{
		{Path: c.Path, Mode: "ro", Readable: true, Writable: false, ReadDurationMs: 12, CheckedAt: time.Now()},
	}})
	if !runFileCheck("pod-1", "app", c, reg) {
		t.Fatal("readable, non-writable path must pass in read mode")
	}
	got := fileResults(reg)
	if len(got) != 1 || !got[0].Passed || got[0].Message != "File check passed" || got[0].Name != "blog.conf" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestFileCheckModes(t *testing.T) {
	cases := []struct {
		mode     string
		readable bool
		writable bool
		want     bool
		msg      string
	}{
		{"read", true, false, true, "File check passed"},
		{"ro", true, false, true, "File check passed"},
		{"", true, false, true, "File check passed"},
		{"read", true, true, false, "unexpected write access (expected read-only)"},
		{"read", false, false, false, "not readable"},
		{"write", true, true, true, "File check passed"},
		{"rw", true, true, true, "File check passed"},
		{"write", true, false, false, "not writable"},
		{"write", false, false, false, "not readable"},
	}
	for _, tc := range cases {
		reg := newFileTestRegistry()
		reg.AddScriptPayload("pod-1", models.ScriptPayload{Pod: "pod-1", Container: "app", FileAccessTargets: []models.FileAccessTarget{
			{Path: "/data", Readable: tc.readable, Writable: tc.writable, CheckedAt: time.Now()},
		}})
		got := runFileCheck("pod-1", "app", ResolvedCheck{Name: "n", Kind: "file", Path: "/data", Mode: tc.mode}, reg)
		if got != tc.want {
			t.Errorf("mode=%q r=%v w=%v: passed=%v want %v", tc.mode, tc.readable, tc.writable, got, tc.want)
		}
		res := fileResults(reg)
		if len(res) != 1 || res[0].Message != tc.msg {
			t.Errorf("mode=%q r=%v w=%v: message %+v want %q", tc.mode, tc.readable, tc.writable, res, tc.msg)
		}
	}
}

func TestNormalizeFileMode(t *testing.T) {
	for in, want := range map[string]string{
		"": "ro", "read": "ro", "READ": "ro", "ro": "ro", "readable": "ro", " read ": "ro",
		"write": "rw", "rw": "rw", "writable": "rw", "RW": "rw", "read-write": "rw",
	} {
		if got := models.NormalizeFileMode(in); got != want {
			t.Errorf("NormalizeFileMode(%q)=%q want %q", in, got, want)
		}
	}
}
