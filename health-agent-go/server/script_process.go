package server

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/health"
	"github.com/fx/health-agent-go/models"
)

// apacheDirectiveLineActive returns false for blank lines and Apache line comments (# after leading spaces/tabs).
func apacheDirectiveLineActive(line string) bool {
	t := strings.TrimLeft(strings.TrimRight(line, "\r"), " \t")
	return t != "" && !strings.HasPrefix(t, "#")
}

// ProcessScriptPayloadHealthChecks parses script payload (ProxyPass, HAProxy, fileAccessTargets) and adds HTTP/file checks to registry (same as Java addScriptPayload).
// Uses registry effective pod name so checks are stored under the same key GetOwnHealthBaseChecks returns (manager callback will include them).
func (s *Server) ProcessScriptPayloadHealthChecks(p *models.ScriptPayload) {
	if p == nil {
		return
	}
	pod := s.Registry.EffectivePodName()
	if pod == "" {
		pod = p.Pod
	}
	if pod == "" {
		return
	}
	container := p.Container
	if container == "" {
		container = pod
	}

	// 0) Seed file access targets from YAML health checks (HC_CHECKS_JSON kind=file). Same as Java addScriptPayload: addFileAccessTarget for each loadFileChecks() file check so script-config returns them and agent.sh runs mount checks.
	checks, err := health.Load()
	if err == nil {
		for _, c := range checks {
			if c.Kind != "file" || c.Path == "" {
				continue
			}
			s.Registry.AddFileAccessTarget(pod, container, models.FileAccessTarget{Path: c.Path, Mode: models.NormalizeFileMode(c.Mode)})
		}
	}

	// ProxyPass: line-anchored; per-line scan with apacheDirectiveLineActive skips # ... comments.
	proxyPassRe := regexp.MustCompile(`(?i)^[ \t]*ProxyPass(?:Reverse)?\s+\S+\s+(https?://[^\s"#]+)`)
	// Directory: line-anchored so #<Directory "..."> is never matched (Directory regex without ^ matched inside comments).
	dirRe := regexp.MustCompile(`(?i)^[ \t]*<Directory\s+([^>]+)>`)
	// Alias: Alias /url "path" or Alias /url /path; optional Apache line comment at end
	aliasRe := regexp.MustCompile(`(?i)^[ \t]*Alias\s+\S+\s+(?:"([^"]+)"|([^"\s#]+))\s*(?:#.*)?\s*$`)
	// DocumentRoot: vhosts often list web roots only here (no matching Alias/Directory)
	documentRootRe := regexp.MustCompile(`(?i)^[ \t]*DocumentRoot\s+(?:"([^"]+)"|'([^']+)'|([^#\s]+))\s*(?:#.*)?\s*$`)
	// HAProxy (line-anchored; leading whitespace is spaces/tabs only so \n cannot be swallowed)
	haproxyOptRe := regexp.MustCompile(`(?i)^[ \t]*option\s+httpchk(?:\s+GET\s+(\S+))?`)
	haproxySrvRe := regexp.MustCompile(`(?i)^[ \t]*server\s+\S+\s+([^\s:]+):(\d+)`)
	haproxyTplRe := regexp.MustCompile(`(?i)^[ \t]*server-template\s+\S+\s+\S+\s+([^\s:]+):(\d+)`)
	haproxyBackRe := regexp.MustCompile(`(?i)\s*(backend|listen)\s+\S+`)

	addedHTTP := make(map[string]bool)
	addedPaths := make(map[string]string) // path → source config file that first added it
	derivedChecks := make([]models.HealthCheck, 0, 64)

	normPath := func(path string) string {
		path = strings.TrimSpace(path)
		if len(path) > 1 && strings.HasSuffix(path, "/") {
			path = path[:len(path)-1]
		}
		return path
	}

	// 1) Parse files: Directory, Alias, ProxyPass, HAProxy (same order as Java addScriptPayload)
	for _, f := range p.Files {
		if f.Base64 == "" {
			continue
		}
		dec, err := base64.StdEncoding.DecodeString(f.Base64)
		if err != nil {
			continue
		}
		content := string(dec)
		fileName := filepath.Base(f.Path)

		// 1a–1c) Apache directives: scan line-by-line so # comments and #<Directory> do not create mounts/HTTP checks.
		for _, rawLine := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
			line := strings.TrimRight(rawLine, "\r")
			if !apacheDirectiveLineActive(line) {
				continue
			}
			// 1a) <Directory "…"> → add file access target (ro)
			for _, m := range dirRe.FindAllStringSubmatch(line, -1) {
				if len(m) < 2 {
					continue
				}
				raw := strings.TrimSpace(m[1])
				if (strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`)) || (strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'")) {
					raw = strings.TrimSpace(raw[1 : len(raw)-1])
				}
				dirPath := normPath(raw)
				if dirPath != "" {
					if _, ok := addedPaths[dirPath]; !ok {
						addedPaths[dirPath] = f.Path
						s.Registry.AddFileAccessTarget(pod, container, models.FileAccessTarget{Path: dirPath, Mode: "ro"})
					}
				}
			}
			// 1b) Alias
			for _, m := range aliasRe.FindAllStringSubmatch(line, -1) {
				if len(m) < 3 {
					continue
				}
				targetPath := normPath(m[1])
				if targetPath == "" {
					targetPath = normPath(m[2])
				}
				if targetPath != "" {
					if _, ok := addedPaths[targetPath]; !ok {
						addedPaths[targetPath] = f.Path
						s.Registry.AddFileAccessTarget(pod, container, models.FileAccessTarget{Path: targetPath, Mode: "ro"})
					}
				}
			}
			// 1b2) DocumentRoot
			for _, m := range documentRootRe.FindAllStringSubmatch(line, -1) {
				if len(m) < 4 {
					continue
				}
				targetPath := normPath(m[1])
				if targetPath == "" {
					targetPath = normPath(m[2])
				}
				if targetPath == "" {
					targetPath = normPath(m[3])
				}
				if targetPath != "" {
					if _, ok := addedPaths[targetPath]; !ok {
						addedPaths[targetPath] = f.Path
						s.Registry.AddFileAccessTarget(pod, container, models.FileAccessTarget{Path: targetPath, Mode: "ro"})
					}
				}
			}
			// 1c) ProxyPass
			for _, m := range proxyPassRe.FindAllStringSubmatch(line, -1) {
				if len(m) < 2 {
					continue
				}
				urlStr := strings.TrimSpace(m[1])
				if urlStr == "" || addedHTTP[urlStr] {
					continue
				}
				addedHTTP[urlStr] = true
				if h, ok := health.BuildScriptHTTPCheck(urlStr); ok {
					derivedChecks = append(derivedChecks, h)
				}
			}
		}

		// HAProxy: option httpchk [GET path] + server/server-template host:port
		isHaproxy := strings.Contains(strings.ToLower(f.Path), "haproxy") ||
			strings.EqualFold(fileName, "haproxy.cfg") ||
			strings.EqualFold(f.Type, "haproxy") ||
			(strings.HasSuffix(strings.ToLower(fileName), ".cfg") &&
				(strings.Contains(strings.ToLower(content), "option httpchk") ||
					strings.Contains(strings.ToLower(content), "backend ") ||
					strings.Contains(strings.ToLower(content), "listen ")))
		if !isHaproxy {
			continue
		}
		currentPath := "/"
		for _, rawLine := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
			line := strings.TrimRight(rawLine, "\r")
			ts := strings.TrimSpace(line)
			if ts == "" || strings.HasPrefix(ts, "#") || strings.HasPrefix(ts, ";") {
				continue
			}
			if haproxyBackRe.MatchString(line) {
				currentPath = "/"
			}
			if opt := haproxyOptRe.FindStringSubmatch(line); len(opt) > 1 && opt[1] != "" {
				p := strings.TrimSpace(opt[1])
				if p != "" && !strings.HasPrefix(p, "/") {
					p = "/" + p
				}
				if p != "" {
					currentPath = p
				}
			}
			var host, port string
			if srv := haproxySrvRe.FindStringSubmatch(line); len(srv) >= 3 {
				host, port = srv[1], srv[2]
			} else if tpl := haproxyTplRe.FindStringSubmatch(line); len(tpl) >= 3 {
				host, port = tpl[1], tpl[2]
			}
			if host != "" && port != "" {
				fullURL := "http://" + host + ":" + port + currentPath
				if addedHTTP[fullURL] {
					continue
				}
				addedHTTP[fullURL] = true
				if h, ok := health.BuildScriptHTTPCheck(fullURL); ok {
					derivedChecks = append(derivedChecks, h)
				}
			}
		}
	}

	// 1.5) Do NOT add "Pending" file checks for paths we haven't received from the script yet.
	// After redeploy, the script hasn't run; sending passed=false would show failed + create incidents.
	// Only add file checks in step 2 when we have actual script results.

	// 2) FileAccessTargets -> file checks (same as Java). Skip test-only paths so manager does not create incidents for them.
	// ConfigFile is set per-path from addedPaths (which file yielded this path in step 1).
	// Using a single p.Files[0] for all checks caused every check to be attributed to the alphabetically-first
	// conf file, so ConfigFile was wrong and manager-side config exclusion could drop vhosts-derived mounts.
	for _, t := range p.FileAccessTargets {
		if t.Path == "" {
			continue
		}
		// Test-only path: we post test-result in handleScriptCallback; do not add to health checks so manager won't create an incident.
		if s.Registry.GetTestIdForPath(t.Path) != "" {
			continue
		}
		// Exact same log as Java AgentRegistry: "Adding file access target: " + path + " with mode: " + mode
			config.LogDebugf("Adding file access target: %s with mode: %s", t.Path, t.Mode)
		passed := models.FileAccessPassed(t.Mode, t.Readable, t.Writable)
		h := models.HealthCheck{
			Type:          "file",
			Name:          "", // auto-detected from config → must stay empty so sync sends all paths to manager (CheckIsAllowed)
			ConfigFile:    addedPaths[t.Path], // per-path source file; empty for CIFS auto-discovered mounts (never excluded)
			Path:          t.Path,
			Mode:          t.Mode,
			Duration:      t.ReadDurationMs,
			Time:          p.Timestamp,
			Passed:        passed,
			Message:       "",
			MountSource:   strings.TrimSpace(t.MountSource),
			MountSourceIP: strings.TrimSpace(t.MountSourceIP),
		}
		if passed {
			h.Message = "File check passed"
		} else if t.TimedOut {
			// Use whichever operation actually hung (read or write)
			d := t.ReadDurationMs
			if t.WriteDurationMs > d {
				d = t.WriteDurationMs
			}
			if d > 0 {
				h.Message = fmt.Sprintf("timed out after %dms", d)
			} else {
				h.Message = "timed out"
			}
		} else {
			mode := models.NormalizeFileMode(t.Mode)
			if mode == "rw" {
				var parts []string
				if !t.Readable {
					s := "not readable"
					if t.ReadDurationMs > 0 {
						s += fmt.Sprintf(" (%dms)", t.ReadDurationMs)
					}
					parts = append(parts, s)
				}
				if !t.Writable {
					s := "not writable"
					if t.WriteDurationMs > 0 {
						s += fmt.Sprintf(" (%dms)", t.WriteDurationMs)
					}
					parts = append(parts, s)
				}
				if len(parts) > 0 {
					h.Message = strings.Join(parts, ", ")
				} else {
					h.Message = "File check failed"
				}
			} else {
				// ro mode: fails if not readable, or unexpectedly writable
				if !t.Readable {
					s := "not readable"
					if t.ReadDurationMs > 0 {
						s += fmt.Sprintf(" (%dms)", t.ReadDurationMs)
					}
					h.Message = s
				} else if t.Writable {
					h.Message = "unexpected write access (expected read-only)"
				} else {
					h.Message = "File check failed"
				}
			}
		}
		derivedChecks = append(derivedChecks, h)
	}
	// Replace script-derived checks atomically so removed/excluded config sources cannot leave stale checks.
	s.Registry.ReplaceScriptDerivedChecks(pod, container, derivedChecks)
	config.LogDebugf("ProcessScriptPayloadHealthChecks: pod=%s, added HTTP targets=%d, file targets=%d", pod, len(addedHTTP), len(p.FileAccessTargets))

	// Store auto-detected Spring Boot datasources (sent by health-agent.sh after scanning the application JAR)
	if len(p.SpringDatasources) > 0 {
		s.Registry.SetSpringDatasources(pod, container, p.SpringDatasources)
		config.LogInfof("[script-callback] auto-detected %d Spring datasource(s) for %s/%s", len(p.SpringDatasources), pod, container)
	}
}
