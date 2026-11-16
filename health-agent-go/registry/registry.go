package registry

import (
	"log"
	"strings"
	"sync"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/models"
)

// Registry holds agent state (health checks, script payloads, inactive keys, pod->deployment).
type Registry struct {
	mu       sync.RWMutex
	cfg      *config.Config
	// pod -> container -> []HealthCheck (slice for JSON array)
	healthBaseChecks map[string]map[string][]models.HealthCheck
	// pod -> container -> ScriptPayload
	scriptPayloads map[string]map[string]models.ScriptPayload
	// pod -> container -> []FileAccessTarget (flattened for getFileAccessTargets)
	fileAccessTargets map[string]map[string][]models.FileAccessTarget
	// pod -> deployment (from peer sync or own)
	podToDeployment map[string]string
	// deployment -> set of inactive check keys (from manager)
	inactiveCheckKeysByDeployment map[string]map[string]struct{}
	// deployment -> set of allowed check keys (checks present in manager: DB + YAML; agent only sends/shows these)
	allowedCheckKeysByDeployment map[string]map[string]struct{}
	// paths from manager (config-paths)
	paths map[string]struct{}
	// excluded config paths from manager (config exclusions)
	excludedPaths map[string]struct{}
	// cluster object from manager callback response
	clusterObject *models.ClusterObject
	// manual health checks from manager DB (active deployment health checks), pulled and run constantly like HC_CHECKS_JSON
	managerHealthChecks []models.JsonCheck
	// runtime override from POST /api/set-pod-name (for TestScript / local dev; Java AgentInit.test() does the same)
	podNameOverride    string
	containerOverride  string
	deploymentOverride string
	// pendingTestPaths: testId -> path info (for TestScript: when script-callback has file result for this path, POST /api/test-result to manager)
	pendingTestPaths map[string]*pendingTestPathEntry
	// pod -> container -> set(script check key). Keys that came from script payload parsing; replaced on every script callback.
	scriptDerivedCheckKeys map[string]map[string]map[string]struct{}
	// pod -> container -> []SpringDatasource (auto-detected from JAR by health-agent.sh)
	springDatasources map[string]map[string][]models.SpringDatasource
	// Monitor endpoint messages fetched from manager settings (GET /api/settings/monitor-messages).
	// Defaults match the manager's DEFAULT_MONITOR_PASS / DEFAULT_MONITOR_FAIL constants.
	monitorPassMsg string
	monitorFailMsg string
	// deployment -> team name (fetched from manager GET /api/teams/assignments on every sync).
	// Used by /monitor?team=X and /status?team=X to filter results to a single team's deployments.
	teamAssignments map[string]string
	// team name -> hex color (e.g. "#4f87c5"). Fetched together with teamAssignments.
	teamColors map[string]string
	// lastManagerRttMs is the round-trip time of the most recent successful callback POST to
	// the manager, measured by the agent (time.Since(start).Milliseconds() around client.Do).
	// Zero until the first successful callback.
	lastManagerRttMs int64
	// actionLogs is a ring buffer of action invocation results to be drained
	// and sent in the next AgentPayload callback to the manager.
	actionLogs []models.ActionLogEntry
}

type pendingTestPathEntry struct {
	testId         string
	path           string
	mode           string
	podName        string
	containerName  string
	isExistingPath bool
}

// Default paths (fallback if manager is not available). Same as Java AgentRegistry.
var defaultPaths = []string{
	"/etc/apache2/apache2.conf",
	"/etc/httpd/conf.d/",
	"/usr/local/apache2/conf/conf.d/",
	"/usr/local/apache2/conf/extra/",
	"/etc/haproxy/haproxy.cfg",
	"/usr/local/etc/haproxy/haproxy.cfg",
	"/etc/nginx/nginx.conf",
	"/usr/local/tomcat/conf/",
}

// New creates a registry with config.
func New(cfg *config.Config) *Registry {
	paths := make(map[string]struct{})
	for _, p := range defaultPaths {
		paths[p] = struct{}{}
	}
	return &Registry{
		cfg:                           cfg,
		healthBaseChecks:              make(map[string]map[string][]models.HealthCheck),
		scriptPayloads:                make(map[string]map[string]models.ScriptPayload),
		fileAccessTargets:             make(map[string]map[string][]models.FileAccessTarget),
		podToDeployment:               make(map[string]string),
		monitorPassMsg:                "@@monitor:pass@@",
		monitorFailMsg:                "@@monitor:fail@@",
		teamAssignments:               make(map[string]string),
		teamColors:                    make(map[string]string),
		inactiveCheckKeysByDeployment: make(map[string]map[string]struct{}),
		allowedCheckKeysByDeployment:  make(map[string]map[string]struct{}),
		paths:                         paths,
		excludedPaths:                 make(map[string]struct{}),
		managerHealthChecks:           nil,
		pendingTestPaths:              make(map[string]*pendingTestPathEntry),
		scriptDerivedCheckKeys:        make(map[string]map[string]map[string]struct{}),
		springDatasources:             make(map[string]map[string][]models.SpringDatasource),
	}
}

func normalizePathKey(path string) string {
	return strings.TrimSpace(strings.ToLower(path))
}

func (r *Registry) isExcludedPathLocked(path string) bool {
	key := normalizePathKey(path)
	if key == "" {
		return false
	}
	_, ok := r.excludedPaths[key]
	return ok
}

func (r *Registry) isFileCheckExcludedLocked(h models.HealthCheck) bool {
	if !strings.EqualFold(h.Type, "file") {
		return false
	}
	if r.isExcludedPathLocked(h.Path) {
		return true
	}
	if r.isExcludedPathLocked(h.ConfigFile) {
		return true
	}
	return false
}

// IsExcludedPath returns true when path is excluded by manager config exclusions.
func (r *Registry) IsExcludedPath(path string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isExcludedPathLocked(path)
}

// SetExcludedPaths stores exclusions from manager and removes already-collected excluded data.
func (r *Registry) SetExcludedPaths(paths []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]struct{})
	for _, p := range paths {
		key := normalizePathKey(p)
		if key != "" {
			next[key] = struct{}{}
		}
	}
	r.excludedPaths = next
	r.applyExclusionsLocked()
}

func (r *Registry) applyExclusionsLocked() {
	// Non-destructive: keep raw script payload/check state in agent.
	// Manager/UI applies exclusion visibility and can instantly restore on undo without waiting for a new script cycle.
}

func scriptDerivedCheckKey(h models.HealthCheck) string {
	return strings.ToLower(strings.TrimSpace(h.Type)) + "|" + getCheckIdentifier(h)
}

// ReplaceScriptDerivedChecks atomically replaces previous script-derived checks for pod/container.
// Non-script checks remain untouched.
func (r *Registry) ReplaceScriptDerivedChecks(pod, container string, checks []models.HealthCheck) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.healthBaseChecks[pod] == nil {
		r.healthBaseChecks[pod] = make(map[string][]models.HealthCheck)
	}
	if r.scriptDerivedCheckKeys[pod] == nil {
		r.scriptDerivedCheckKeys[pod] = make(map[string]map[string]struct{})
	}
	oldSet := r.scriptDerivedCheckKeys[pod][container]
	existing := r.healthBaseChecks[pod][container]

	// Keep only non-script-derived checks from previous state.
	filtered := make([]models.HealthCheck, 0, len(existing)+len(checks))
	for _, h := range existing {
		if oldSet != nil {
			if _, ok := oldSet[scriptDerivedCheckKey(h)]; ok {
				continue
			}
		}
		filtered = append(filtered, h)
	}

	// Append new script-derived checks and track keys.
	newSet := make(map[string]struct{})
	for _, h := range checks {
		if r.isFileCheckExcludedLocked(h) {
			continue
		}
		k := scriptDerivedCheckKey(h)
		if _, exists := newSet[k]; exists {
			continue
		}
		newSet[k] = struct{}{}
		filtered = append(filtered, h)
	}

	r.healthBaseChecks[pod][container] = filtered
	r.scriptDerivedCheckKeys[pod][container] = newSet
}

// SetManagerHealthChecks stores active deployment health checks from manager (GET /api/deployment-health-checks/yaml). Run constantly like env HC_CHECKS_JSON.
func (r *Registry) SetManagerHealthChecks(checks []models.JsonCheck) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.managerHealthChecks = checks
}

// GetManagerHealthChecks returns a copy of manager-sourced health checks (manual checks saved in DB, pulled into agent).
func (r *Registry) GetManagerHealthChecks() []models.JsonCheck {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.managerHealthChecks) == 0 {
		return nil
	}
	cp := make([]models.JsonCheck, len(r.managerHealthChecks))
	copy(cp, r.managerHealthChecks)
	return cp
}

// SetPodNameOverride sets the current pod/container/deployment (from POST /api/set-pod-name). Same as Java AgentInit.test().
func (r *Registry) SetPodNameOverride(pod, container, deployment string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.podNameOverride = pod
	r.containerOverride = container
	r.deploymentOverride = deployment
}

// effectivePodNameLocked returns effective pod name; caller must hold r.mu (at least RLock).
func (r *Registry) effectivePodNameLocked() string {
	if r.podNameOverride != "" {
		return r.podNameOverride
	}
	return r.cfg.PodName
}

// EffectivePodName returns override if set, else config pod name (for /agent and "own" payloads).
func (r *Registry) EffectivePodName() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.effectivePodNameLocked()
}

// effectiveContainerNameLocked returns effective container name; caller must hold r.mu (at least RLock).
func (r *Registry) effectiveContainerNameLocked() string {
	if r.containerOverride != "" {
		return r.containerOverride
	}
	return r.cfg.ContainerName
}

// EffectiveDeployment returns override if set, else config deployment.
func (r *Registry) EffectiveDeployment() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.deploymentOverride != "" {
		return r.deploymentOverride
	}
	return r.cfg.Deployment
}

// EffectiveContainerName returns override if set, else config container name (for HC checks / status when running as sidecar).
func (r *Registry) EffectiveContainerName() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.effectiveContainerNameLocked()
}

// AddHealthCheck adds or updates a health check for pod/container (same identifier logic as Java).
func (r *Registry) AddHealthCheck(pod, container, checkType string, h models.HealthCheck) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.healthBaseChecks[pod] == nil {
		r.healthBaseChecks[pod] = make(map[string][]models.HealthCheck)
	}
	if r.healthBaseChecks[pod][container] == nil {
		r.healthBaseChecks[pod][container] = nil
	}
	id := getCheckIdentifier(h)
	checks := r.healthBaseChecks[pod][container]
	var found bool
	for i, c := range checks {
		if getCheckIdentifier(c) == id {
			checks[i] = h
			found = true
			config.LogDebugf("Updating existing health check: %s", id)
			break
		}
	}
	if !found {
		r.healthBaseChecks[pod][container] = append(checks, h)
	}
	healthStr := "false"
	if h.Passed {
		healthStr = "true"
	}
	config.LogDebugf("Adding health check for container: %s type: %s health: %s", container, checkType, healthStr)
	if !h.Passed {
		detail := h.Response
		if detail == "" {
			detail = h.Message
		}
		config.WriteFailLogf("health FAIL pod=%s container=%s kind=%s type=%s name=%s path=%q url=%q code=%q detail=%q",
			pod, container, checkType, h.Type, h.Name, h.Path, h.URL, h.ResponseCode, detail)
	}
}

// PruneHTTPCheckByURL removes a single HTTP check entry (matched by URL) from pod/container.
// Used when a DB override with the same name but a different URL displaces an annotation check —
// the old annotation check will never run again, so its stale result must be removed proactively.
func (r *Registry) PruneHTTPCheckByURL(pod, container, rawURL string) {
	if rawURL == "" {
		return
	}
	norm := normURLForIdentity(rawURL)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.healthBaseChecks[pod] == nil || r.healthBaseChecks[pod][container] == nil {
		return
	}
	checks := r.healthBaseChecks[pod][container]
	out := make([]models.HealthCheck, 0, len(checks))
	for _, h := range checks {
		if h.Type == "http" && normURLForIdentity(h.URL) == norm {
			config.LogDebugf("Pruning displaced HTTP check URL=%s from registry", rawURL)
			continue
		}
		out = append(out, h)
	}
	r.healthBaseChecks[pod][container] = out
}

// PruneCheckByName removes a single check entry matched by name from pod/container.
// Used when Pass 2 endpoint-identity dedup displaces an annotation check whose name differs
// from the DB override — the agent stops running it, so its stale result must be cleared
// so the manager also stops seeing it (same behaviour as a YAML rename + redeploy).
func (r *Registry) PruneCheckByName(pod, container, name string) {
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.healthBaseChecks[pod] == nil || r.healthBaseChecks[pod][container] == nil {
		return
	}
	checks := r.healthBaseChecks[pod][container]
	out := make([]models.HealthCheck, 0, len(checks))
	for _, h := range checks {
		if h.Name == name {
			config.LogDebugf("Pruning displaced check name=%q from registry (replaced by DB override)", name)
			continue
		}
		out = append(out, h)
	}
	r.healthBaseChecks[pod][container] = out
}

// AliasHealthCheck copies the latest registry result for srcName to each dstName in aliases.
// Used when two manager DB checks share the same endpoint (same URL/host) under different names:
// the runner only executes the winning (newer) check, but both DB check records need a result
// in the registry so that neither appears as Pending in the dashboard.
// The alias entries are marked synthetic so a future prune of srcName does not cascade.
func (r *Registry) AliasHealthCheck(pod, container, srcName string, aliases []string) {
	if len(aliases) == 0 || srcName == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.healthBaseChecks[pod] == nil || r.healthBaseChecks[pod][container] == nil {
		return
	}
	// Find the source check result.
	var src *models.HealthCheck
	for i := range r.healthBaseChecks[pod][container] {
		if r.healthBaseChecks[pod][container][i].Name == srcName {
			src = &r.healthBaseChecks[pod][container][i]
			break
		}
	}
	if src == nil {
		return // source not yet in registry (e.g. empty-URL early-return)
	}
	checks := r.healthBaseChecks[pod][container]
	for _, aliasName := range aliases {
		aliased := *src
		aliased.Name = aliasName
		// Update existing alias entry if present, otherwise append.
		found := false
		for i := range checks {
			if checks[i].Name == aliasName {
				checks[i] = aliased
				found = true
				break
			}
		}
		if !found {
			checks = append(checks, aliased)
		}
		config.LogDebugf("Aliased check result src=%q → alias=%q (both DB checks share same endpoint)", srcName, aliasName)
	}
	r.healthBaseChecks[pod][container] = checks
}

// PruneSpringMysqlChecksOutsideSet drops mysql checks named spring-db* that are no longer merged from
// Spring datasource discovery (e.g. user excluded the virtual BOOT-INF properties path). Those checks
// are skipped in the run loop so AddHealthCheck is never called again; without pruning, /status would
// show the last result forever.
func (r *Registry) PruneSpringMysqlChecksOutsideSet(pod, container string, keepNamesLower map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.healthBaseChecks[pod] == nil || r.healthBaseChecks[pod][container] == nil {
		return
	}
	checks := r.healthBaseChecks[pod][container]
	out := make([]models.HealthCheck, 0, len(checks))
	for _, h := range checks {
		if h.Type != "mysql" {
			out = append(out, h)
			continue
		}
		n := strings.TrimSpace(h.Name)
		if !strings.HasPrefix(strings.ToLower(n), "spring-db") {
			out = append(out, h)
			continue
		}
		if _, ok := keepNamesLower[strings.ToLower(n)]; ok {
			out = append(out, h)
		}
	}
	r.healthBaseChecks[pod][container] = out
}

// normURLForIdentity normalizes HTTP URL for use as check identity (same URL = one check, avoid duplicate when one source has no name and another has name).
func normURLForIdentity(url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}
	url = strings.ToLower(url)
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	for len(url) > 1 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	return url
}

func normHostForIdentity(host string) string {
	return strings.ToLower(strings.TrimSpace(host))
}

// getCheckIdentifier returns a stable id so the same logical check (same URL/host/path) is updated, not duplicated.
// For HTTP/MySQL we use normalized URL/host so that a check with no name (script) and same URL with name (manager) map to one slot and don't duplicate on /monitor or incidents.
func getCheckIdentifier(h models.HealthCheck) string {
	if h.Type == "file" && h.Path != "" {
		return "file:" + strings.ToLower(strings.TrimSpace(h.Path))
	}
	if h.Type == "http" && h.URL != "" {
		return "http:" + normURLForIdentity(h.URL)
	}
	if h.Type == "mysql" {
		// Use name as primary key so multiple checks to the same host (e.g. master + slave) stay distinct.
		if h.Name != "" {
			return "mysql:" + strings.ToLower(strings.TrimSpace(h.Name))
		}
		if h.URL != "" {
			return "mysql:" + normHostForIdentity(h.URL)
		}
	}
	// Fallback for unknown types
	if h.Name != "" {
		return "name:" + h.Name
	}
	return h.Type + ":" + h.URL
}

// ReplaceHealthCheckForPod replaces all health checks for a pod (from peer sync; use for other pods only).
func (r *Registry) ReplaceHealthCheckForPod(pod string, containerMap map[string][]models.HealthCheck) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if containerMap == nil {
		delete(r.healthBaseChecks, pod)
		return
	}
	r.healthBaseChecks[pod] = make(map[string][]models.HealthCheck)
	for c, list := range containerMap {
		filtered := make([]models.HealthCheck, 0, len(list))
		for _, h := range list {
			filtered = append(filtered, h)
		}
		r.healthBaseChecks[pod][c] = filtered
	}
}

// MergeHealthChecksForPod adds/updates health checks for a pod from sync without removing existing checks (same as Java: script-callback + sync combine).
func (r *Registry) MergeHealthChecksForPod(pod string, containerMap map[string][]models.HealthCheck) {
	if containerMap == nil {
		return
	}
	for c, list := range containerMap {
		for _, h := range list {
			checkType := h.Type
			if checkType == "" {
				checkType = "http"
				if h.Path != "" {
					checkType = "file"
				}
				if h.URL != "" && (h.User != "" || h.Db != "") {
					checkType = "mysql"
				}
			}
			r.AddHealthCheck(pod, c, checkType, h)
		}
	}
}

// ReplaceScriptPayloadsForPod replaces script payloads for a pod (from sync). containerMap is container -> ScriptPayload.
func (r *Registry) ReplaceScriptPayloadsForPod(pod string, containerMap map[string]models.ScriptPayload) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if containerMap == nil {
		delete(r.scriptPayloads, pod)
		return
	}
	r.scriptPayloads[pod] = make(map[string]models.ScriptPayload)
	for c, p := range containerMap {
		r.scriptPayloads[pod][c] = p
	}
}

// SetDeploymentForPod sets deployment name for a pod.
func (r *Registry) SetDeploymentForPod(pod, deployment string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.podToDeployment[pod] = deployment
}

// GetDeploymentForPod returns deployment for pod or "".
func (r *Registry) GetDeploymentForPod(pod string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.podToDeployment[pod]
}

// PutInactiveCheckKeysForDeployment stores inactive keys for a deployment (from manager).
func (r *Registry) PutInactiveCheckKeysForDeployment(deployment string, keys map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inactiveCheckKeysByDeployment[deployment] = keys
}

// GetInactiveCheckKeysForDeployment returns inactive keys for deployment (nil = empty).
func (r *Registry) GetInactiveCheckKeysForDeployment(deployment string) map[string]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.inactiveCheckKeysByDeployment[deployment]
}

// PutAllowedCheckKeysForDeployment stores allowed keys for a deployment (checks present in manager: DB + YAML).
func (r *Registry) PutAllowedCheckKeysForDeployment(deployment string, keys map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.allowedCheckKeysByDeployment == nil {
		r.allowedCheckKeysByDeployment = make(map[string]map[string]struct{})
	}
	r.allowedCheckKeysByDeployment[deployment] = keys
}

// GetAllowedCheckKeysForDeployment returns allowed keys for deployment (nil = empty; agent shows/sends all when empty).
func (r *Registry) GetAllowedCheckKeysForDeployment(deployment string) map[string]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.allowedCheckKeysByDeployment == nil {
		return nil
	}
	return r.allowedCheckKeysByDeployment[deployment]
}

// GetDeploymentNames returns all deployment names we know (for manager sync).
func (r *Registry) GetDeploymentNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{})
	for _, d := range r.podToDeployment {
		if d != "" {
			seen[d] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	return out
}

// GetHealthBaseChecks returns a deep copy of the full map (for /agent, /status). Caller gets a consistent snapshot so /status shows all pods including peers.
func (r *Registry) GetHealthBaseChecks() map[string]map[string][]models.HealthCheck {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]map[string][]models.HealthCheck, len(r.healthBaseChecks))
	for pod, containerMap := range r.healthBaseChecks {
		out[pod] = make(map[string][]models.HealthCheck, len(containerMap))
		for container, list := range containerMap {
			cp := make([]models.HealthCheck, len(list))
			copy(cp, list)
			out[pod][container] = cp
		}
	}
	return out
}

// GetOwnHealthBaseChecks returns only this pod's health checks (for manager/agent sync).
// Returns a deep copy so callback payload is a consistent snapshot (same as GetHealthBaseChecks).
func (r *Registry) GetOwnHealthBaseChecks() map[string]map[string][]models.HealthCheck {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]map[string][]models.HealthCheck)
	pod := r.podNameOverride
	if pod == "" {
		pod = r.cfg.PodName
	}
	if pod == "" {
		return out
	}
	cm, ok := r.healthBaseChecks[pod]
	if !ok || cm == nil {
		return out
	}
	out[pod] = make(map[string][]models.HealthCheck, len(cm))
	for c, list := range cm {
		cp := make([]models.HealthCheck, len(list))
		copy(cp, list)
		out[pod][c] = cp
	}
	return out
}

// GetScriptPayloads returns full map (for /agent, /script).
func (r *Registry) GetScriptPayloads() map[string]map[string]models.ScriptPayload {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scriptPayloads
}

// GetOwnScriptPayloads returns only this pod's script payloads, with merged fileAccessTargets (for manager/agent sync).
func (r *Registry) GetOwnScriptPayloads() map[string]map[string]models.ScriptPayload {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]map[string]models.ScriptPayload)
	pod := r.podNameOverride
	if pod == "" {
		pod = r.cfg.PodName
	}
	if pod == "" {
		return out
	}
	sp, ok := r.scriptPayloads[pod]
	if !ok || sp == nil {
		return out
	}
	out[pod] = make(map[string]models.ScriptPayload)
	fa := r.fileAccessTargets[pod]
	for c, p := range sp {
		combined := p
		combinedTargets := p.FileAccessTargets
		if combinedTargets == nil {
			combinedTargets = []models.FileAccessTarget{}
		}
		paths := make(map[string]struct{})
		for _, t := range combinedTargets {
			if t.Path != "" {
				paths[t.Path] = struct{}{}
			}
		}
		if fa != nil && fa[c] != nil {
			for _, t := range fa[c] {
				if t.Path != "" {
					if _, exists := paths[t.Path]; exists {
						continue
					}
					paths[t.Path] = struct{}{}
					combinedTargets = append(combinedTargets, t)
				}
			}
		}
		combined.FileAccessTargets = combinedTargets
		out[pod][c] = combined
	}
	return out
}

// AddScriptPayload stores script payload for pod (from script-callback). Container key from payload.Container or pod.
func (r *Registry) AddScriptPayload(pod string, p models.ScriptPayload) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scriptPayloads[pod] == nil {
		r.scriptPayloads[pod] = make(map[string]models.ScriptPayload)
	}
	container := p.Container
	if container == "" {
		container = pod
	}
	r.scriptPayloads[pod][container] = p
	// Merge file access targets from payload
	if r.fileAccessTargets[pod] == nil {
		r.fileAccessTargets[pod] = make(map[string][]models.FileAccessTarget)
	}
	if r.fileAccessTargets[pod][container] == nil {
		r.fileAccessTargets[pod][container] = nil
	}
	existing := r.fileAccessTargets[pod][container]
	paths := make(map[string]struct{})
	for _, t := range existing {
		if t.Path != "" {
			paths[t.Path] = struct{}{}
		}
	}
	for _, t := range p.FileAccessTargets {
		if t.Path != "" {
			if _, exists := paths[t.Path]; exists {
				continue
			}
			paths[t.Path] = struct{}{}
			existing = append(existing, t)
		}
	}
	r.fileAccessTargets[pod][container] = existing
}

// GetScriptFileTarget returns what the in-container script last measured for path (readable/writable), looking
// in the given container's payload first and then in any other container of the pod. ok=false until the script
// has reported the path.
func (r *Registry) GetScriptFileTarget(pod, container, path string) (models.FileAccessTarget, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	byContainer := r.scriptPayloads[pod]
	if byContainer == nil {
		return models.FileAccessTarget{}, false
	}
	find := func(p models.ScriptPayload) (models.FileAccessTarget, bool) {
		for _, t := range p.FileAccessTargets {
			if t.Path == path && !t.CheckedAt.IsZero() {
				return t, true
			}
		}
		return models.FileAccessTarget{}, false
	}
	if p, ok := byContainer[container]; ok {
		if t, ok := find(p); ok {
			return t, true
		}
	}
	for c, p := range byContainer {
		if c == container {
			continue
		}
		if t, ok := find(p); ok {
			return t, true
		}
	}
	return models.FileAccessTarget{}, false
}

// AddFileAccessTarget adds a file access target for pod/container.
func (r *Registry) AddFileAccessTarget(pod, container string, t models.FileAccessTarget) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fileAccessTargets[pod] == nil {
		r.fileAccessTargets[pod] = make(map[string][]models.FileAccessTarget)
	}
	if r.fileAccessTargets[pod][container] == nil {
		r.fileAccessTargets[pod][container] = []models.FileAccessTarget{}
	}
	for _, e := range r.fileAccessTargets[pod][container] {
		if e.Path == t.Path {
			return
		}
	}
	r.fileAccessTargets[pod][container] = append(r.fileAccessTargets[pod][container], t)
}

// AddPendingTestPath stores path→testId for TestScript: when script-callback reports file result for this path, agent POSTs /api/test-result to manager.
func (r *Registry) AddPendingTestPath(testId, path, mode, podName, containerName string, isExistingPath bool) {
	if testId == "" || path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingTestPaths == nil {
		r.pendingTestPaths = make(map[string]*pendingTestPathEntry)
	}
	r.pendingTestPaths[testId] = &pendingTestPathEntry{
		testId:         testId,
		path:           path,
		mode:           mode,
		podName:        podName,
		containerName:  containerName,
		isExistingPath: isExistingPath,
	}
	log.Printf("Added pending test path: testId=%s path=%s mode=%s", testId, path, mode)
}

// GetTestIdForPath returns testId if this path was added as a pending test (for script-callback → POST test-result).
func (r *Registry) GetTestIdForPath(path string) string {
	if path == "" {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.pendingTestPaths {
		if e != nil && e.path == path {
			return e.testId
		}
	}
	return ""
}

// RemoveTestPath removes pending test path after result submitted; optionally removes from fileAccessTargets if not isExistingPath.
func (r *Registry) RemoveTestPath(testId string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.pendingTestPaths[testId]
	delete(r.pendingTestPaths, testId)
	if e == nil {
		return
	}
	if e.isExistingPath {
		return
	}
	if r.fileAccessTargets[e.podName] != nil && r.fileAccessTargets[e.podName][e.containerName] != nil {
		list := r.fileAccessTargets[e.podName][e.containerName]
		filtered := list[:0]
		for _, t := range list {
			if t.Path != e.path {
				filtered = append(filtered, t)
			}
		}
		r.fileAccessTargets[e.podName][e.containerName] = filtered
		log.Printf("Removed test path from fileAccessTargets: %s", e.path)
	}
}

// refOrValueStr extracts a string from manager check value (path/url etc): plain string or {"value": "..."}.
func refOrValueStr(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if m, ok := v.(map[string]interface{}); ok {
		if val, ok := m["value"].(string); ok {
			return val
		}
	}
	return ""
}

// GetFileAccessTargets returns all file access targets for pod (all containers flattened).
// Includes manager DB file checks (from deployment-health-checks/yaml) for this agent's pod so the script can run them and post results via script-callback.
func (r *Registry) GetFileAccessTargets(pod, container string) []models.FileAccessTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []models.FileAccessTarget
	pathsSeen := make(map[string]struct{})
	fa := r.fileAccessTargets[pod]
	if fa != nil {
		for c, list := range fa {
			if container != "" && c != container {
				continue
			}
			for _, t := range list {
				if t.Path != "" {
					key := strings.TrimSpace(strings.ToLower(t.Path))
					if _, excluded := r.excludedPaths[key]; excluded {
						continue
					}
					if _, ok := pathsSeen[key]; !ok {
						pathsSeen[key] = struct{}{}
						out = append(out, t)
					}
				}
			}
		}
	}
	// Include manager DB file checks so manual mount checks get run by the script and stop being "pending"
	effPod := r.effectivePodNameLocked()
	effCont := r.effectiveContainerNameLocked()
	if effPod != "" && pod == effPod && (container == "" || container == effCont) && len(r.managerHealthChecks) > 0 {
		for i := range r.managerHealthChecks {
			c := &r.managerHealthChecks[i]
			if strings.ToLower(strings.TrimSpace(c.Kind)) != "file" || c.Value == nil {
				continue
			}
			path := refOrValueStr(c.Value.Path)
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			key := strings.ToLower(path)
			if _, excluded := r.excludedPaths[key]; excluded {
				continue
			}
			if _, ok := pathsSeen[key]; ok {
				continue
			}
			pathsSeen[key] = struct{}{}
			mode := strings.TrimSpace(c.Value.Mode)
			if mode == "" {
				mode = "ro"
			}
			out = append(out, models.FileAccessTarget{Path: path, Mode: mode})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// GetPaths returns paths from manager (config-paths).
func (r *Registry) GetPaths() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.paths))
	for p := range r.paths {
		out = append(out, p)
	}
	return out
}

// SetPaths sets paths from manager.
func (r *Registry) SetPaths(paths []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = make(map[string]struct{})
	for _, p := range paths {
		r.paths[p] = struct{}{}
	}
}

// SetClusterObject stores cluster object from manager callback.
func (r *Registry) SetClusterObject(co *models.ClusterObject) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clusterObject = co
}

// GetClusterObject returns cluster object.
func (r *Registry) GetClusterObject() *models.ClusterObject {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clusterObject
}

// SetSpringDatasources stores auto-detected Spring Boot datasources for a pod/container (from health-agent.sh JAR scan).
func (r *Registry) SetSpringDatasources(pod, container string, ds []models.SpringDatasource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.springDatasources[pod] == nil {
		r.springDatasources[pod] = make(map[string][]models.SpringDatasource)
	}
	r.springDatasources[pod][container] = ds
}

// SetMonitorMessages stores pass/fail messages fetched from manager settings.
func (r *Registry) SetMonitorMessages(passMsg, failMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if passMsg != "" {
		r.monitorPassMsg = passMsg
	}
	if failMsg != "" {
		r.monitorFailMsg = failMsg
	}
}

// GetMonitorPassMsg returns the current monitor pass message (default @@monitor:pass@@).
func (r *Registry) GetMonitorPassMsg() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.monitorPassMsg == "" {
		return "@@monitor:pass@@"
	}
	return r.monitorPassMsg
}

// GetMonitorFailMsg returns the current monitor fail message (default @@monitor:fail@@).
func (r *Registry) GetMonitorFailMsg() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.monitorFailMsg == "" {
		return "@@monitor:fail@@"
	}
	return r.monitorFailMsg
}

// SetTeamAssignments replaces the full deployment→team map fetched from manager.
func (r *Registry) SetTeamAssignments(assignments map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if assignments == nil {
		r.teamAssignments = make(map[string]string)
		return
	}
	r.teamAssignments = assignments
}

// GetTeamForDeployment returns the team name for the given deployment, or "" if unassigned.
func (r *Registry) GetTeamForDeployment(deployment string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.teamAssignments[deployment]
}

// GetTeamAssignments returns a copy of the full deployment→team map.
func (r *Registry) GetTeamAssignments() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cp := make(map[string]string, len(r.teamAssignments))
	for k, v := range r.teamAssignments {
		cp[k] = v
	}
	return cp
}

// SetTeamColors replaces the full team→color map fetched from manager.
func (r *Registry) SetTeamColors(colors map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if colors == nil {
		r.teamColors = make(map[string]string)
		return
	}
	r.teamColors = colors
}

// GetTeamColor returns the hex color for the given team name, or "" if none set.
func (r *Registry) GetTeamColor(teamName string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.teamColors[teamName]
}

// GetTeamColors returns a copy of the full team→color map.
func (r *Registry) GetTeamColors() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cp := make(map[string]string, len(r.teamColors))
	for k, v := range r.teamColors {
		cp[k] = v
	}
	return cp
}

// GetSpringDatasources returns auto-detected Spring Boot datasources for a pod/container.
func (r *Registry) GetSpringDatasources(pod, container string) []models.SpringDatasource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if m := r.springDatasources[pod]; m != nil {
		return m[container]
	}
	return nil
}

// SetManagerRttMs records the measured round-trip time (ms) of the most recent successful
// callback POST to the manager.  Called by manager_sync after each successful client.Do.
func (r *Registry) SetManagerRttMs(rttMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastManagerRttMs = rttMs
}

// GetManagerRttMs returns the last measured manager callback RTT in milliseconds.
// Returns 0 if no successful callback has been recorded yet (agent just started).
func (r *Registry) GetManagerRttMs() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastManagerRttMs
}

// AppendActionLog appends one action invocation result to the ring buffer (max 100 entries).
// The buffer is drained on every successful callback POST to the manager.
func (r *Registry) AppendActionLog(entry models.ActionLogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actionLogs = append(r.actionLogs, entry)
	// Keep ring buffer bounded
	if len(r.actionLogs) > 100 {
		r.actionLogs = r.actionLogs[len(r.actionLogs)-100:]
	}
}

// DrainActionLogs atomically returns all pending action log entries and clears the buffer.
// Called by manager_sync just before building the callback payload.
func (r *Registry) DrainActionLogs() []models.ActionLogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.actionLogs) == 0 {
		return nil
	}
	drained := make([]models.ActionLogEntry, len(r.actionLogs))
	copy(drained, r.actionLogs)
	r.actionLogs = r.actionLogs[:0]
	return drained
}
