package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/health"
	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
	"github.com/fx/health-agent-go/sync"
)

//go:embed templates/*
var embedTemplates embed.FS

// Deployment ReplicaSet suffix (e.g. web-apache-f5f95867-9lw5l -> web-apache)
var podNameSuffixRegex = regexp.MustCompile(`-[a-z0-9]{9,10}-[a-z0-9]{5}$`)
// StatefulSet ordinal suffix (e.g. web-site-0 -> web-site) so deployment name matches manager
var statefulSetOrdinalRegex = regexp.MustCompile(`-\d+$`)

// Server holds config and registry for HTTP handlers.
type Server struct {
	Cfg      *config.Config
	Registry *registry.Registry
}

// Run starts the HTTP server on cfg.ServerPort.
func (s *Server) Run() error {
	mux := http.NewServeMux()
	s.register(mux)
	// Serve same static assets as Java agent (from static/assets, same as health-agent src/main/resources/static/assets)
	if _, err := os.Stat("static/assets"); err == nil {
		mux.Handle("/assets/", http.StripPrefix("/assets", http.FileServer(http.Dir("static/assets"))))
	}
	addr := ":" + strconv.Itoa(s.Cfg.ServerPort)
	config.LogWarnf("health-agent listening on %s (local-only routes restricted to loopback: %v)", addr, !listenAll())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second, // script-callback bodies can be large
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	return srv.ListenAndServe()
}

// listenAll restores the old behaviour (every route reachable on the pod IP). Off by default.
func listenAll() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("HEALTH_AGENT_LISTEN_ALL")), "true")
}

// localOnly allows a route only from the pod itself (the in-container script, kubectl exec / port-forward).
// Other pods can reach /health and /api/sync (peer sync) on the pod IP but nothing else.
func localOnly(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !listenAll() {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			ip := net.ParseIP(host)
			if err != nil || ip == nil || !ip.IsLoopback() {
				http.NotFound(w, r)
				return
			}
		}
		fn(w, r)
	}
}

func (s *Server) register(mux *http.ServeMux) {
	// Reachable from other pods: kubelet probes and agent-to-agent sync.
	mux.HandleFunc("/health", method("GET", s.handleHealth))
	mux.HandleFunc("/api/sync", method("POST", s.handleSync))
	// Everything else is for the in-pod script and for debugging via kubectl exec/port-forward: loopback only.
	mux.HandleFunc("/api/script-config", localOnly(method("GET", s.handleScriptConfig)))
	mux.HandleFunc("/api/script-callback", localOnly(method("POST", s.handleScriptCallback)))
	mux.HandleFunc("/api/software-probes", localOnly(method("GET", s.handleSoftwareProbes)))
	mux.HandleFunc("/agent", localOnly(method("GET", s.handleAgent)))
	mux.HandleFunc("/script", localOnly(method("GET", s.handleScript)))
	mux.HandleFunc("/cluster", localOnly(method("GET", s.handleCluster)))
	mux.HandleFunc("/status", localOnly(method("GET", s.handleStatus)))
	mux.HandleFunc("/monitor", localOnly(method("GET", s.handleMonitor)))
	mux.HandleFunc("/api/set-pod-name", localOnly(method("POST", s.handleSetPodName)))
	mux.HandleFunc("/api/fake-config", localOnly(method("GET", s.handleFakeConfig)))
	mux.HandleFunc("/api/test-add-path", localOnly(method("POST", s.handleTestAddPath)))
}

// method wraps a handler to allow only the given HTTP method (Go 1.21 compat).
func method(allow string, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != allow {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fn(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleScriptConfig(w http.ResponseWriter, r *http.Request) {
	pod := r.URL.Query().Get("pod")
	container := r.URL.Query().Get("container")
	paths := s.Registry.GetPaths()
	targets := s.Registry.GetFileAccessTargets(pod, container)
	if targets == nil {
		targets = []models.FileAccessTarget{} // Java returns empty array, not null
	}
	cfg := models.ScriptConfig{Paths: paths, FileAccessTargets: targets}
	// Exact same log as Java RestController: "ScriptConfig: {}" (JSON)
	if b, err := json.Marshal(cfg); err == nil {
		config.LogDebugf("ScriptConfig: %s", string(b))
	}
	writeJSON(w, cfg)
}

// handleSoftwareProbes serves the software-detection rules to health-agent.sh (plain text, see health.SoftwareProbeSpec).
// Empty body = nothing to collect (detection disabled).
func (s *Server) handleSoftwareProbes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, health.SoftwareProbeSpec())
}

func (s *Server) handleScriptCallback(w http.ResponseWriter, r *http.Request) {
	var p models.ScriptPayload
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err == nil {
		err = json.Unmarshal(body, &p)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Optional software facts from inside the container (ScriptPayload has no such field, so they are never
	// forwarded to the manager as-is; they are turned into detectedSoftware instead). Never fails the callback.
	health.IngestSoftwareFacts(body, p.Container)
	pod := p.Pod
	if pod == "" {
		http.Error(w, "pod required", http.StatusBadRequest)
		return
	}
	s.Registry.AddScriptPayload(pod, p)
	if p.Error != "" {
		container := p.Container
		if container == "" {
			container = pod
		}
		config.LogWarnf("[script-callback] container %s reported error: %s", container, p.Error)
		s.Registry.AddHealthCheck(pod, container, "script-error", models.HealthCheck{
			Type:    "file",
			Name:    "script-" + container,
			Passed:  false,
			Message: p.Error,
			Time:    p.Timestamp,
		})
	}
	s.ProcessScriptPayloadHealthChecks(&p)
	// If any file result was a pending test (TestScript injected via test-add-path), POST /api/test-result to manager
	base := managerBaseURL(s.Cfg.ManagerURL)
	if base != "" && len(p.FileAccessTargets) > 0 {
		client := &http.Client{Timeout: 10 * time.Second}
		for _, t := range p.FileAccessTargets {
			testId := s.Registry.GetTestIdForPath(t.Path)
			if testId == "" {
				continue
			}
			passed := false
			passed = models.FileAccessPassed(t.Mode, t.Readable, t.Writable)
			message := "File check passed"
			if !passed {
				message = "File check failed"
			}
			duration := t.ReadDurationMs
			if duration <= 0 && t.WriteDurationMs > 0 {
				duration = t.WriteDurationMs
			}
			body := map[string]interface{}{
				"testId":   testId,
				"passed":   passed,
				"message":  message,
				"duration": duration,
			}
			payload, _ := json.Marshal(body)
			req, err := http.NewRequest(http.MethodPost, base+"/api/test-result", bytes.NewReader(payload))
			if err != nil {
				config.LogErrorf("[script-callback] test-result request build: %v", err)
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				config.LogErrorf("[script-callback] POST test-result: %v", err)
				continue
			}
			resp.Body.Close()
			s.Registry.RemoveTestPath(testId)
			if !passed {
				config.LogWarnf("[script-callback] test result failed testId=%s", testId)
			} else {
				config.LogDebugf("[script-callback] submitted test result testId=%s passed=%v", testId, passed)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

// managerBaseURL returns scheme+host+port from MANAGER_URL (same as sync.managerBaseURL).
func managerBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexAny(raw, "/?"); i > 0 {
			return strings.TrimSuffix(raw[:i], "/")
		}
		return strings.TrimSuffix(raw, "/")
	}
	u.Path = ""
	u.RawQuery = ""
	u.RawPath = ""
	return strings.TrimSuffix(u.String(), "/")
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var payload models.AgentPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Never apply peer sync for own pod — agent never gets own data from peer sync; own data comes from script-callback / manager / local checks.
	if payload.PodName == s.Registry.EffectivePodName() {
		config.LogDebugf("Sync: ignoring sync for own pod %s", payload.PodName)
		w.WriteHeader(http.StatusOK)
		return
	}
	if payload.DeploymentName != "" {
		s.Registry.SetDeploymentForPod(payload.PodName, payload.DeploymentName)
	}
	if payload.ScriptPayloads != nil {
		if cm, ok := payload.ScriptPayloads[payload.PodName]; ok {
			s.Registry.ReplaceScriptPayloadsForPod(payload.PodName, cm)
		}
	}
	if payload.HealthBaseChecks != nil {
		if cm, ok := payload.HealthBaseChecks[payload.PodName]; ok {
			n := 0
			for _, list := range cm {
				n += len(list)
			}
			s.Registry.ReplaceHealthCheckForPod(payload.PodName, cm)
			config.LogDebugf("Sync: replaced health checks for peer pod %s (%d checks)", payload.PodName, n)
		}
	}
	// Apply cluster object from peer so all agents show same pod list on /status (canonical list propagates without waiting for manager callback)
	if payload.ClusterObject != nil && payload.ClusterObject.Deployments != nil && len(payload.ClusterObject.Deployments) > 0 {
		s.Registry.SetClusterObject(payload.ClusterObject)
		config.LogDebugf("Sync: updated cluster object from peer %s (%d deployments)", payload.PodName, len(payload.ClusterObject.Deployments))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	payload := models.AgentPayload{
		PodName:              s.Registry.EffectivePodName(),
		DeploymentName:       s.Registry.EffectiveDeployment(),
		Version:              s.Cfg.AgentVersion,
		MemoryUsageBytes:     sync.AgentProcessMemorySys(),
		ScriptPayloads:       s.Registry.GetScriptPayloads(),
		HealthBaseChecks:     s.Registry.GetHealthBaseChecks(),
		ManagerHealthChecks:  s.Registry.GetManagerHealthChecks(),
	}
	writeJSON(w, payload)
}

func (s *Server) handleScript(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.Registry.GetScriptPayloads())
}

func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	co := s.Registry.GetClusterObject()
	if co == nil {
		writeJSON(w, &models.ClusterObject{})
		return
	}
	writeJSON(w, co)
}

func (s *Server) handleSetPodName(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	podName := body["podName"]
	if podName == "" {
		http.Error(w, "podName is required", http.StatusBadRequest)
		return
	}
	container := body["container"]
	if container == "" {
		container = podName
	}
	deployment := body["deployment"]
	if deployment == "" {
		// Strip Deployment ReplicaSet suffix or StatefulSet ordinal so manager deployment name matches
		deployment = podNameSuffixRegex.ReplaceAllString(podName, "")
		if deployment == podName {
			deployment = statefulSetOrdinalRegex.ReplaceAllString(podName, "")
		}
	}
	s.Registry.SetPodNameOverride(podName, container, deployment)
	s.Registry.SetDeploymentForPod(podName, deployment)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleFakeConfig(w http.ResponseWriter, r *http.Request) {
	// No-op for compatibility
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleTestAddPath(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path, _ := body["path"].(string)
	mode := "ro"
	if m, ok := body["mode"].(string); ok && m != "" {
		mode = m
	}
	podName, _ := body["podName"].(string)
	containerName, _ := body["containerName"].(string)
	testId, _ := body["testId"].(string)
	isExistingPath := false
	if b, ok := body["isExistingPath"].(bool); ok {
		isExistingPath = b
	}
	if path == "" || podName == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if containerName == "" {
		containerName = s.Registry.EffectiveContainerName()
	}
	if containerName == "" {
		containerName = s.Registry.EffectiveDeployment()
	}
	if containerName == "" {
		containerName = podName
	}
	s.Registry.AddFileAccessTarget(podName, containerName, models.FileAccessTarget{Path: path, Mode: mode})
	if testId != "" && containerName != "" {
		s.Registry.AddPendingTestPath(testId, path, mode, podName, containerName, isExistingPath)
	}
	config.LogDebugf("Added test path from /api/test-add-path: pod=%s container=%s path=%s mode=%s", podName, containerName, path, mode)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	teamFilter := strings.TrimSpace(r.URL.Query().Get("team"))
	root := s.Registry.GetHealthBaseChecks()
	var rows []models.HealthRow
	for podName, containerMap := range root {
		deploymentName := s.Registry.GetDeploymentForPod(podName)
		if deploymentName == "" && podName == s.Registry.EffectivePodName() {
			deploymentName = s.Registry.EffectiveDeployment()
		}
		// Team filter: skip pods whose deployment is not in the requested team.
		if teamFilter != "" {
			podTeam := s.Registry.GetTeamForDeployment(deploymentName)
			if !strings.EqualFold(podTeam, teamFilter) {
				continue
			}
		}
		inactiveKeys := s.Registry.GetInactiveCheckKeysForDeployment(deploymentName)
		allowedKeys := s.Registry.GetAllowedCheckKeysForDeployment(deploymentName)
		seenForPod := make(map[string]struct{})
		for containerName, checks := range containerMap {
			for _, check := range checks {
				// Show only checks present in manager (active or inactive). When allowed set is empty, show all.
				if len(allowedKeys) > 0 && !sync.CheckIsAllowed(check, allowedKeys) {
					continue
				}
				// Deduplicate across containers: same check (type+identity) should not appear twice for the same pod.
				dedupKey := check.Type + "|" + checkIdentityForDedup(check)
				if _, seen := seenForPod[dedupKey]; seen {
					continue
				}
				seenForPod[dedupKey] = struct{}{}
				rows = append(rows, toRow(podName, containerName, deploymentName, check, inactiveKeys))
			}
		}
	}
	// Sort: failed first, then stable by Type/Name/FilePath/URL so order doesn't jump on refresh
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Passed != rows[j].Passed {
			return !rows[i].Passed && rows[j].Passed
		}
		if rows[i].Type != rows[j].Type {
			return rows[i].Type < rows[j].Type
		}
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		if rows[i].FilePath != rows[j].FilePath {
			return rows[i].FilePath < rows[j].FilePath
		}
		return rows[i].URL < rows[j].URL
	})
	// Group by pod (from healthBaseChecks)
	groupedByPod := make(map[string][]models.HealthRow)
	for _, row := range rows {
		groupedByPod[row.Pod] = append(groupedByPod[row.Pod], row)
	}
	// Canonical pod list from cluster object (manager callback) so every agent shows the same pods regardless of which agent is hit
	type podDeployment struct {
		Pod        string
		Deployment string
	}
	var allPodsOrdered []podDeployment
	if co := s.Registry.GetClusterObject(); co != nil && co.Deployments != nil {
		var depNames []string
		for d := range co.Deployments {
			depNames = append(depNames, d)
		}
		sort.Strings(depNames)
		for _, depName := range depNames {
			// Team filter on cluster list: skip deployments not in the requested team.
			if teamFilter != "" {
				podTeam := s.Registry.GetTeamForDeployment(depName)
				if !strings.EqualFold(podTeam, teamFilter) {
					continue
				}
			}
			dep := co.Deployments[depName]
			if dep.Pods != nil {
				var podNames []string
				for p := range dep.Pods {
					podNames = append(podNames, p)
				}
				sort.Strings(podNames)
				for _, podName := range podNames {
					allPodsOrdered = append(allPodsOrdered, podDeployment{Pod: podName, Deployment: depName})
				}
			}
		}
	}
	// If no cluster object yet, fall back to pods we have in healthBaseChecks (existing behavior)
	if len(allPodsOrdered) == 0 {
		for pod, rowList := range groupedByPod {
			dep := ""
			if len(rowList) > 0 {
				dep = rowList[0].Deployment
			}
			allPodsOrdered = append(allPodsOrdered, podDeployment{Pod: pod, Deployment: dep})
		}
		sort.Slice(allPodsOrdered, func(i, j int) bool {
			if allPodsOrdered[i].Deployment != allPodsOrdered[j].Deployment {
				return allPodsOrdered[i].Deployment < allPodsOrdered[j].Deployment
			}
			return allPodsOrdered[i].Pod < allPodsOrdered[j].Pod
		})
	}
	// Build groupedByPodOrdered from canonical list so pod order is consistent across agents.
	// Only include pods that have health data (connected agents); skip pods in cluster that never reported.
	type podGroup struct {
		Pod        string
		Deployment string
		Team       string // team name for this deployment, or ""
		TeamColor  string // hex color for the team, or ""
		Uptime     string // pod uptime from cluster startTime, or empty if unknown
		Rows       []models.HealthRow
	}
	co := s.Registry.GetClusterObject()
	inCluster := make(map[string]bool)
	for _, pd := range allPodsOrdered {
		inCluster[pd.Pod] = true
	}
	var groupedByPodOrdered []podGroup
	for _, pd := range allPodsOrdered {
		rowList := groupedByPod[pd.Pod]
		if len(rowList) == 0 {
			continue // skip pods with no health data (not connected / no agent)
		}
		uptime := getPodUptime(co, pd.Deployment, pd.Pod)
		team := s.Registry.GetTeamForDeployment(pd.Deployment)
		teamColor := ""
		if team != "" {
			teamColor = s.Registry.GetTeamColor(team)
		}
		groupedByPodOrdered = append(groupedByPodOrdered, podGroup{Pod: pd.Pod, Deployment: pd.Deployment, Team: team, TeamColor: teamColor, Uptime: uptime, Rows: rowList})
	}
	// Include pods that have health data but are not in cluster (e.g. local-go-0 when testing locally)
	for pod, rowList := range groupedByPod {
		if inCluster[pod] || len(rowList) == 0 {
			continue
		}
		dep := ""
		if len(rowList) > 0 {
			dep = rowList[0].Deployment
		}
		uptime := getPodUptime(co, dep, pod)
		team := s.Registry.GetTeamForDeployment(dep)
		teamColor := ""
		if team != "" {
			teamColor = s.Registry.GetTeamColor(team)
		}
		groupedByPodOrdered = append(groupedByPodOrdered, podGroup{Pod: pod, Deployment: dep, Team: team, TeamColor: teamColor, Uptime: uptime, Rows: rowList})
	}
	// Summary: pods per deployment (so you can compare counts across refreshes/agents)
	podsByDeployment := make(map[string]int)
	for _, rowList := range groupedByPod {
		dep := ""
		if len(rowList) > 0 {
			dep = rowList[0].Deployment
		}
		podsByDeployment[dep]++
	}
	var depNames []string
	for d := range podsByDeployment {
		depNames = append(depNames, d)
	}
	sort.Strings(depNames)
	var summaryParts []string
	for _, d := range depNames {
		summaryParts = append(summaryParts, d+": "+strconv.Itoa(podsByDeployment[d]))
	}
	podsSummary := "0 pods"
	if len(summaryParts) > 0 {
		podsSummary = strings.Join(summaryParts, ", ")
	}
	totalPods := len(groupedByPodOrdered)
	tmpl, err := template.ParseFS(embedTemplates, "templates/status.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := struct {
		PodName             string
		PodsSummary         string
		TotalPods           int
		GroupedByPodOrdered []podGroup
	}{PodName: s.Registry.EffectivePodName(), PodsSummary: podsSummary, TotalPods: totalPods, GroupedByPodOrdered: groupedByPodOrdered}
	w.Header().Set("Content-Type", "text/html;charset=UTF-8")
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleMonitor returns plain text for external/3rd-party monitoring (e.g. SMS). If any check failed,
// body contains the configured fail message (default @@monitor:fail@@) and short lines listing failures.
// Supports ?team=<name> to filter to deployments assigned to that team.
// Uses same canonical pod order as /status (from cluster object) so the list is stable and does not flicker.
func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	teamFilter := strings.TrimSpace(r.URL.Query().Get("team"))
	root := s.Registry.GetHealthBaseChecks()
	// Canonical pod order from cluster (same as /status) so list is stable across refreshes and agents.
	type podDeployment struct {
		Pod        string
		Deployment string
	}
	var allPodsOrdered []podDeployment
	if co := s.Registry.GetClusterObject(); co != nil && co.Deployments != nil {
		var depNames []string
		for d := range co.Deployments {
			depNames = append(depNames, d)
		}
		sort.Strings(depNames)
		for _, depName := range depNames {
			dep := co.Deployments[depName]
			if dep.Pods != nil {
				var podNames []string
				for p := range dep.Pods {
					podNames = append(podNames, p)
				}
				sort.Strings(podNames)
				for _, podName := range podNames {
					allPodsOrdered = append(allPodsOrdered, podDeployment{Pod: podName, Deployment: depName})
				}
			}
		}
	}
	if len(allPodsOrdered) == 0 {
		var podNames []string
		for p := range root {
			podNames = append(podNames, p)
		}
		sort.Strings(podNames)
		for _, podName := range podNames {
			dep := s.Registry.GetDeploymentForPod(podName)
			if dep == "" && podName == s.Registry.EffectivePodName() {
				dep = s.Registry.EffectiveDeployment()
			}
			allPodsOrdered = append(allPodsOrdered, podDeployment{Pod: podName, Deployment: dep})
		}
	}
	var rows []models.HealthRow
	for _, pd := range allPodsOrdered {
		containerMap := root[pd.Pod]
		if containerMap == nil {
			continue
		}
		deploymentName := pd.Deployment
		if deploymentName == "" {
			deploymentName = s.Registry.GetDeploymentForPod(pd.Pod)
			if deploymentName == "" && pd.Pod == s.Registry.EffectivePodName() {
				deploymentName = s.Registry.EffectiveDeployment()
			}
		}
		// Team filter: skip deployments that don't belong to the requested team.
		if teamFilter != "" {
			podTeam := s.Registry.GetTeamForDeployment(deploymentName)
			if !strings.EqualFold(podTeam, teamFilter) {
				continue
			}
		}
		inactiveKeys := s.Registry.GetInactiveCheckKeysForDeployment(deploymentName)
		seenForPod := make(map[string]struct{})
		for containerName, checks := range containerMap {
			for _, check := range checks {
				dedupKey := check.Type + "|" + checkIdentityForDedup(check)
				if _, seen := seenForPod[dedupKey]; seen {
					continue
				}
				seenForPod[dedupKey] = struct{}{}
				rows = append(rows, toRow(pd.Pod, containerName, deploymentName, check, inactiveKeys))
			}
		}
	}
	var failedRows []models.HealthRow
	for _, row := range rows {
		if row.Inactive || row.Passed {
			continue
		}
		failedRows = append(failedRows, row)
	}
	// Stable sort: pod first, then type/name/url so order is same after refresh.
	sort.Slice(failedRows, func(i, j int) bool {
		if failedRows[i].Pod != failedRows[j].Pod {
			return failedRows[i].Pod < failedRows[j].Pod
		}
		if failedRows[i].Type != failedRows[j].Type {
			return failedRows[i].Type < failedRows[j].Type
		}
		if failedRows[i].Name != failedRows[j].Name {
			return failedRows[i].Name < failedRows[j].Name
		}
		if failedRows[i].FilePath != failedRows[j].FilePath {
			return failedRows[i].FilePath < failedRows[j].FilePath
		}
		return failedRows[i].URL < failedRows[j].URL
	})
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	servedBy := s.Registry.EffectivePodName()
	if servedBy == "" {
		servedBy = "(unknown)"
	}
	passMsg := s.Registry.GetMonitorPassMsg()
	failMsg := s.Registry.GetMonitorFailMsg()
	var out []string
	out = append(out, "Served by: "+servedBy)
	if teamFilter != "" {
		out = append(out, "Team: "+teamFilter)
	}
	if len(failedRows) == 0 {
		out = append(out, passMsg)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Join(out, "\n")))
		return
	}
	// Group by pod, then by type. Each pod lists its own failures independently.
	type typeSection struct {
		typ   string
		lines []string
	}
	type podFails struct {
		pod      string
		sections []typeSection
	}
	var groups []podFails
	var curPod *podFails
	var curSection *typeSection
	for _, row := range failedRows {
		detail := row.Response
		if row.Code != "" {
			detail = row.Code
		}
		if len(detail) > 40 {
			detail = detail[:37] + "..."
		}
		line := row.Name
		if row.URL != "" {
			line += " " + row.URL
		}
		if row.FilePath != "" {
			line += " " + row.FilePath
		}
		if detail != "" && detail != "0" {
			line += " " + detail
		}
		line = strings.TrimSpace(line)
		if curPod == nil || curPod.pod != row.Pod {
			groups = append(groups, podFails{pod: row.Pod, sections: nil})
			curPod = &groups[len(groups)-1]
			curSection = nil
		}
		if curSection == nil || curSection.typ != row.Type {
			curPod.sections = append(curPod.sections, typeSection{typ: row.Type, lines: nil})
			curSection = &curPod.sections[len(curPod.sections)-1]
		}
		curSection.lines = append(curSection.lines, "    "+line)
	}
	out = append(out, failMsg)
	for _, g := range groups {
		out = append(out, g.pod)
		for _, sec := range g.sections {
			out = append(out, "  "+sec.typ)
			out = append(out, sec.lines...)
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(strings.Join(out, "\n")))
}

// checkIdentityForDedup returns a stable string identifying a check (same logic as registry.getCheckIdentifier).
// Used to deduplicate the same logical check that arrives under different container names for the same pod.
func checkIdentityForDedup(h models.HealthCheck) string {
	if h.Type == "file" && h.Path != "" {
		return "file:" + strings.ToLower(strings.TrimSpace(h.Path))
	}
	if h.URL != "" {
		u := strings.ToLower(h.URL)
		u = strings.TrimPrefix(u, "https://")
		u = strings.TrimPrefix(u, "http://")
		for len(u) > 1 && u[len(u)-1] == '/' {
			u = u[:len(u)-1]
		}
		return "url:" + u
	}
	if h.Db != "" {
		return "db:" + strings.ToLower(strings.TrimSpace(h.Db))
	}
	return "name:" + strings.ToLower(strings.TrimSpace(h.Name))
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// formatUptime returns a human-readable duration (e.g. "2d 3h", "45m", "30s").
func formatUptime(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	s := int(d.Seconds())
	if s < 60 {
		return strconv.Itoa(s) + "s"
	}
	if s < 3600 {
		return strconv.Itoa(s/60) + "m"
	}
	if s < 86400 {
		h := s / 3600
		m := (s % 3600) / 60
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	}
	days := s / 86400
	rem := s % 86400
	h := rem / 3600
	if h == 0 {
		return strconv.Itoa(days) + "d"
	}
	return strconv.Itoa(days) + "d " + strconv.Itoa(h) + "h"
}

// getPodUptime extracts startTime for the pod from the cluster object (manager sends PodObject.startTime from K8s) and returns formatted uptime, or "" if unknown.
func getPodUptime(co *models.ClusterObject, deployment, podName string) string {
	if co == nil || co.Deployments == nil {
		return ""
	}
	dep, ok := co.Deployments[deployment]
	if !ok || dep.Pods == nil {
		return ""
	}
	podVal, ok := dep.Pods[podName]
	if !ok {
		return ""
	}
	pm, ok := podVal.(map[string]interface{})
	if !ok {
		return ""
	}
	startVal, ok := pm["startTime"]
	if !ok {
		return ""
	}
	startStr, ok := startVal.(string)
	if !ok || startStr == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		// try without fractional seconds
		t, err = time.Parse(time.RFC3339Nano, startStr)
		if err != nil {
			return ""
		}
	}
	return formatUptime(time.Since(t))
}

// formatTimeAgo returns "X sec(s) / minute(s) / hour(s) / day(s) ago" (same as Java).
func formatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	diff := time.Since(t)
	if diff < 0 {
		return "0 secs ago"
	}
	sec := int(diff.Seconds())
	min := sec / 60
	hr := min / 60
	day := hr / 24
	if sec < 60 {
		s := "sec"
		if sec != 1 {
			s = "secs"
		}
		return strconv.Itoa(sec) + " " + s + " ago"
	}
	if min < 60 {
		s := "minute"
		if min != 1 {
			s = "minutes"
		}
		return strconv.Itoa(min) + " " + s + " ago"
	}
	if hr < 24 {
		s := "hour"
		if hr != 1 {
			s = "hours"
		}
		return strconv.Itoa(hr) + " " + s + " ago"
	}
	s := "day"
	if day != 1 {
		s = "days"
	}
	return strconv.Itoa(day) + " " + s + " ago"
}

func nz(s string) string {
	if s == "" {
		return ""
	}
	return strings.TrimSpace(s)
}

// isCheckInactive matches Java MainController.isCheckInactive (same key building).
func isCheckInactive(check models.HealthCheck, inactiveKeys map[string]struct{}) bool {
	if inactiveKeys == nil || len(inactiveKeys) == 0 {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(check.Name))
	switch check.Type {
	case "http":
		url := strings.TrimSpace(check.URL)
		if url == "" {
			return false
		}
		withPort := url
		if check.Port > 0 && !portInURL(url) {
			withPort = url + ":" + strconv.Itoa(check.Port)
		}
		withPort = strings.ToLower(withPort)
		k := "http:" + name + ":" + withPort
		if _, ok := inactiveKeys[k]; ok {
			return true
		}
		if _, ok := inactiveKeys["http::"+withPort]; ok {
			return true
		}
		norm := normHTTPURL(withPort)
		if _, ok := inactiveKeys["http:"+name+":"+norm]; ok {
			return true
		}
		if _, ok := inactiveKeys["http::"+norm]; ok {
			return true
		}
		return false
	case "mysql":
		host := strings.ToLower(strings.TrimSpace(check.URL))
		if host == "" {
			return false
		}
		if _, ok := inactiveKeys["mysql:"+name+":"+host]; ok {
			return true
		}
		without3306 := strings.TrimSuffix(host, ":3306")
		if _, ok := inactiveKeys["mysql:"+name+":"+without3306]; ok {
			return true
		}
		if _, ok := inactiveKeys["mysql:"+name+":"+without3306+":3306"]; ok {
			return true
		}
		if _, ok := inactiveKeys["mysql::"+host]; ok {
			return true
		}
		if _, ok := inactiveKeys["mysql::"+without3306]; ok {
			return true
		}
		if _, ok := inactiveKeys["mysql::"+without3306+":3306"]; ok {
			return true
		}
		return false
	case "file":
		path := strings.ToLower(strings.TrimSpace(check.Path))
		if path == "" {
			return false
		}
		if _, ok := inactiveKeys["file:"+name+":"+path]; ok {
			return true
		}
		if _, ok := inactiveKeys["file::"+path]; ok {
			return true
		}
		return false
	}
	return false
}

func portInURL(u string) bool {
	// Port is in URL if the host part (authority) already contains ":port".
	// Previous logic returned false for URLs ending with "/" (e.g. ...:8081/) and caused double port.
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	host := parsed.Host
	if host == "" && parsed.Opaque == "" {
		// No scheme or weird form; fallback: any ":digits" before path
		if idx := strings.Index(u, "://"); idx >= 0 {
			rest := u[idx+3:]
			if pathStart := strings.Index(rest, "/"); pathStart >= 0 {
				rest = rest[:pathStart]
			}
			return strings.Contains(rest, ":")
		}
		return false
	}
	return strings.Contains(host, ":")
}

func normHTTPURL(u string) string {
	u = strings.ToLower(u)
	if strings.HasPrefix(u, "http://") {
		u = u[7:]
	} else if strings.HasPrefix(u, "https://") {
		u = u[8:]
	}
	for len(u) > 1 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u
}

// toRow builds a HealthRow from a check (same as Java MainController.toRow).
func toRow(podName, containerName, deploymentName string, check models.HealthCheck, inactiveKeys map[string]struct{}) models.HealthRow {
	passed := check.Passed
	inactive := isCheckInactive(check, inactiveKeys)
	typ := check.Type
	name := nz(check.Name)
	filePath := ""
	urlStr := ""
	response := ""
	code := ""

	switch check.Type {
	case "http":
		urlStr = nz(check.URL)
		if check.Port > 0 && !portInURL(urlStr) {
			urlStr = urlStr + ":" + strconv.Itoa(check.Port)
		}
		response = nz(check.Response)
		code = nz(check.ResponseCode)
	case "file":
		filePath = nz(check.Path)
		response = nz(check.Message)
	case "mysql":
		urlStr = nz(check.URL)
		if check.Port > 0 && !portInURL(urlStr) {
			urlStr = urlStr + ":" + strconv.Itoa(check.Port)
		}
		response = nz(check.Response)
	default:
		response = nz(check.Response) + nz(check.Message)
	}

	timeAgo := formatTimeAgo(check.Time)
	return models.HealthRow{
		Pod:        nz(podName),
		Deployment: nz(deploymentName),
		Passed:     passed,
		Inactive:   inactive,
		Type:       typ,
		Name:       name,
		FilePath:   filePath,
		URL:        urlStr,
		Response:   response,
		Code:       code,
		TimeAgo:    timeAgo,
	}
}
