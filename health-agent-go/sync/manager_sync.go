package sync

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/health"
	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

// pendingTestPollInterval is how often the agent polls for pending tests between main sync cycles.
// This ensures tests are picked up within a few seconds regardless of the configured sync interval.
const pendingTestPollInterval = 3 * time.Second

// RunManagerSync runs the manager sync loop: fetch sync intervals from manager, callback + fetch inactive keys, then sleep (respects manager settings).
// Same as Java ManagerSyncTask: fetch sync intervals from manager; use client that does not follow redirects (Java RestTemplate behavior).
func RunManagerSync(cfg *config.Config, reg *registry.Registry, intervals *Intervals) {
	RunManagerSyncWithScanner(cfg, reg, intervals, nil)
}

// RunManagerSyncWithScanner is the same as RunManagerSync but accepts an optional ConnectionScanner
// whose 24-hour union of detected TCP connections is included in every agent callback payload.
func RunManagerSyncWithScanner(cfg *config.Config, reg *registry.Registry, intervals *Intervals, connScanner interface{ GetConnections() []models.DetectedConnection }) {
	if intervals == nil {
		intervals = NewIntervals()
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for {
		intervals.Fetch(managerBaseURL(cfg.ManagerURL), client)
		runManagerOnceWithScanner(cfg, reg, client, connScanner)

		// Between main syncs: poll for pending tests every 3 seconds so tests are
		// picked up quickly regardless of the configured manager sync interval.
		mainInterval := intervals.GetManagerInterval()
		deadline := time.Now().Add(mainInterval)
		for time.Now().Before(deadline) {
			sleep := pendingTestPollInterval
			if remaining := time.Until(deadline); sleep > remaining {
				sleep = remaining
			}
			time.Sleep(sleep)
			if time.Now().Before(deadline) {
				runPendingTestsOnly(cfg, reg, client)
			}
		}
	}
}

// runPendingTestsOnly checks for and runs any pending tests without doing a full manager callback.
// Called between main sync cycles so tests are picked up within a few seconds.
//
// IMPORTANT: only polls for the agent's OWN deployment (plus any explicitly configured
// ADDITIONAL_PENDING_TEST_DEPLOYMENTS).  The full-cycle runManagerOnce also covers every
// deployment in the ClusterObject, but doing that on the fast 3-second inter-cycle path
// was catastrophic at scale: 60 agents × 60 deployments × every 3s = ~1,200 req/s to the
// manager from pending-test polls alone.  The ClusterObject loop stays in runManagerOnce
// (30-second cadence) where the cost is negligible.
func runPendingTestsOnly(cfg *config.Config, reg *registry.Registry, client *http.Client) {
	base := managerBaseURL(cfg.ManagerURL)
	deployment := reg.EffectiveDeployment()
	podName := reg.EffectivePodName()

	// Only own deployment + explicitly listed extras — no ClusterObject expansion here.
	seen := make(map[string]struct{})
	var deployments []string
	add := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" {
			return
		}
		if _, ok := seen[d]; ok {
			return
		}
		seen[d] = struct{}{}
		deployments = append(deployments, d)
	}
	add(deployment)
	for _, d := range strings.Split(cfg.AdditionalPendingTestDeployments, ",") {
		add(d)
	}

	for _, d := range deployments {
		fetchAndRunPendingTests(base, d, podName, reg, client)
	}
}

// managerBaseURL returns only scheme+host+port from MANAGER_URL (same as Java: manager URL is base only).
// If MANAGER_URL is set with a path/query (e.g. .../api/agent-callback?cluster=...), we strip it so we always call the correct endpoint.
func managerBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "http://health-manager.infrastructure.svc.cluster.local:8080"
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Fallback: trim from first "/" or "?"
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

func runManagerOnce(cfg *config.Config, reg *registry.Registry, client *http.Client) {
	runManagerOnceWithScanner(cfg, reg, client, nil)
}

func runManagerOnceWithScanner(cfg *config.Config, reg *registry.Registry, client *http.Client, connScanner interface{ GetConnections() []models.DetectedConnection }) {
	// Callback to manager with own payload (use effective pod/deployment from set-pod-name override if set).
	// Sync every interval even when script payloads are empty so health checks and pod listing update
	// (e.g. help pod — tabs get data once health.checks are configured and script runs).
	// Java: config.getManagerUrl() + "/api/agent-callback?deployment={deployment}" — manager URL is base only
	scriptPayloads := reg.GetOwnScriptPayloads()
	if scriptPayloads == nil || len(scriptPayloads) == 0 {
			config.LogDebugf("Waiting for agent script to register...")
	}
	base := managerBaseURL(cfg.ManagerURL)
	podName := reg.EffectivePodName()
	deployment := reg.EffectiveDeployment()

	// Fetch pending tests from manager: run for our pod from our deployment and from any additional deployments (e.g. help when running one agent locally).
	// No scriptPayloads gate so local agent+manager can run http/mysql pending tests without TestScript.
	pendingDeployments := buildPendingTestDeployments(deployment, reg, cfg.AdditionalPendingTestDeployments)
	for _, d := range pendingDeployments {
		if d != "" {
			fetchAndRunPendingTests(base, d, podName, reg, client)
		}
	}
	healthChecks := reg.GetOwnHealthBaseChecks()
	// Send only checks that exist in the manager (DB + YAML, active or inactive). Deleted checks are removed from agent.
	healthChecks = filterHealthChecksByAllowed(healthChecks, deployment, reg)
	// Always include our pod so the manager can write pod-level history (sparklines). If all checks were filtered out we'd otherwise omit the pod.
	if deployment != "" {
		if healthChecks == nil {
			healthChecks = make(map[string]map[string][]models.HealthCheck)
		}
		if _, hasPod := healthChecks[podName]; !hasPod {
			healthChecks[podName] = make(map[string][]models.HealthCheck)
		}
	}
	payload := models.AgentPayload{
		PodName:            podName,
		DeploymentName:     deployment,
		Version:            cfg.AgentVersion,
		MemoryUsageBytes:   AgentProcessMemorySys(),
		// Include the RTT measured on the previous callback cycle so the manager can
		// track agent-to-manager network latency per pod.  Zero on the very first call.
		ManagerRttMs:       reg.GetManagerRttMs(),
		ScriptPayloads:     scriptPayloads,
		HealthBaseChecks:   healthChecks,
	}
	if connScanner != nil {
		payload.DetectedConnections = connScanner.GetConnections()
		if sw, ok := connScanner.(interface{ GetSoftware() []models.DetectedSoftware }); ok {
			payload.DetectedSoftware = sw.GetSoftware()
		}
	}
	// Drain pending action log entries so the manager can persist them to check_action_log.
	if actionLogs := reg.DrainActionLogs(); len(actionLogs) > 0 {
		payload.ActionLogs = actionLogs
	}
		config.LogDebugf("[manager-sync] callback payload: podName=%q deployment=%q version=%q", podName, deployment, cfg.AgentVersion)
	totalHC := 0
	for p, contMap := range healthChecks {
		for c, list := range contMap {
			n := len(list)
			totalHC += n
				config.LogDebugf("[manager-sync] healthBaseChecks[%q][%q] = %d checks", p, c, n)
			for i, h := range list {
					config.LogDebugf("[manager-sync]   [%d] type=%q name=%q passed=%v url=%q path=%q", i+1, h.Type, h.Name, h.Passed, h.URL, h.Path)
			}
		}
	}
		config.LogDebugf("[manager-sync] healthBaseChecks total=%d", totalHC)
	for p, contMap := range scriptPayloads {
		for c, sp := range contMap {
			n := 0
			if sp.Files != nil {
				n = len(sp.Files)
			}
				config.LogDebugf("[manager-sync] scriptPayloads[%q][%q] = %d files", p, c, n)
		}
	}
	// ── Network RTT via TCP dial ────────────────────────────────────────────
	// Measure pure network latency (TCP 3-way handshake) to the manager host.
	// This is intentionally separate from the HTTP callback POST so manager-side
	// processing time (JSON parsing, DB writes, etc.) does not inflate the reading.
	// No special privileges are needed for a TCP dial inside a Kubernetes pod.
	if tcpRtt := measureTCPRtt(base); tcpRtt > 0 {
		reg.SetManagerRttMs(tcpRtt)
	}

	// Unknown deployment: omit the parameter and let the manager resolve it from the pod name.
	callbackURL := base + "/api/agent-callback"
	if deployment != "" {
		callbackURL += "?deployment=" + url.QueryEscape(deployment)
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, callbackURL, strings.NewReader(string(body)))
	if err != nil {
			config.LogErrorf("Manager sync: failed to build request: %v", err)
	} else {
		req.Header.Set("Content-Type", "application/json")
			config.LogDebugf("synching with manager at %s", callbackURL)
		callbackStart := time.Now()
		resp, err := client.Do(req)
		_ = callbackStart // kept for logging/debug use; RTT is now measured via TCP dial above
		noteManagerHTTPOutcome("callback", err, resp,
			func(prev string) {
				config.LogInfof("Manager sync (callback): OK (recovered, was %s)", prev)
			},
			func() {
				if err != nil {
					config.LogErrorf("Manager sync (callback): %v", err)
				} else if resp != nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
					config.LogWarnf("Manager sync (callback): %d redirect (not following). Use internal cluster URL for MANAGER_URL (e.g. http://health-manager.infrastructure.svc.cluster.local:8080) so manager receives callbacks and pod tabs show data.", resp.StatusCode)
				} else if resp != nil {
					config.LogWarnf("Manager sync (callback): HTTP %d", resp.StatusCode)
				}
			},
		)
		if err != nil {
			// outcome already logged by noteManagerHTTPOutcome — buffer for replay.
			bufferPayload(payload)
		} else {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var co models.ClusterObject
				if json.NewDecoder(resp.Body).Decode(&co) == nil {
					reg.SetClusterObject(&co)
				}
				// Replay any payloads buffered during manager downtime.
				buffered := drainBuffer()
				if len(buffered) > 0 {
					config.LogInfof("Manager sync: replaying %d buffered payload(s) after manager recovery", len(buffered))
					for _, bp := range buffered {
						replayPayload(base, bp, client)
					}
				}
			} else {
				// Non-2xx (e.g. 503 during rolling restart) — buffer for replay.
				bufferPayload(payload)
			}
			resp.Body.Close()
		}
	}

	// Fetch inactive health check keys for own deployment and all deployments we know
	deployments := reg.GetDeploymentNames()
	if deployment != "" {
		found := false
		for _, d := range deployments {
			if d == deployment {
				found = true
				break
			}
		}
		if !found {
			deployments = append(deployments, deployment)
		}
	}
	for _, deployment := range deployments {
		if deployment == "" {
			continue
		}
		inactiveURL := base + "/api/deployment-health-checks?deploymentName=" + url.QueryEscape(deployment)
		req, err := http.NewRequest(http.MethodGet, inactiveURL, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		stateKey := "inactive:" + deployment
		noteManagerHTTPOutcome(stateKey, err, resp,
			func(prev string) {
				config.LogInfof("Manager sync (inactive for %s): OK (recovered, was %s)", deployment, prev)
			},
			func() {
				if err != nil {
					config.LogErrorf("Manager sync (inactive for %s): %v", deployment, err)
				} else if resp != nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
					config.LogWarnf("Manager sync (inactive for %s): %d redirect (not following)", deployment, resp.StatusCode)
				} else if resp != nil {
					config.LogWarnf("Manager sync (inactive for %s): HTTP %d", deployment, resp.StatusCode)
				}
			},
		)
		if err != nil {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			continue
		}
		var result struct {
			Success   bool                     `json:"success"`
			DbChecks  []map[string]interface{} `json:"dbChecks"`
			YamlChecks []map[string]interface{} `json:"yamlChecks"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()
		if !result.Success {
			continue
		}
		// Inactive set: only checks with active == false (for backward compat / UI).
		// If manager returns no dbChecks for this deployment, no checks will show as Inactive on /status (all show active). Ensure cluster is registered and deployment name matches.
		if result.DbChecks == nil || len(result.DbChecks) == 0 {
				config.LogDebugf("Manager sync (inactive for %s): manager returned success but no dbChecks — register this deployment in health-manager and add health checks (some with active=false) so /status shows Inactive correctly", deployment)
		}
		if result.DbChecks != nil {
			keys := buildInactiveCheckKeys(result.DbChecks)
			set := make(map[string]struct{})
			for _, k := range keys {
				set[k] = struct{}{}
			}
			reg.PutInactiveCheckKeysForDeployment(deployment, set)
		}
		// Allowed set: all checks present in manager (DB + YAML). Agent only sends/shows these; deleted checks are dropped.
		allowedSet := buildAllowedCheckKeys(result.DbChecks, result.YamlChecks)
		reg.PutAllowedCheckKeysForDeployment(deployment, allowedSet)
	}

	// Fetch active deployment health checks from manager (manual checks saved in DB). Run constantly in health loop like HC_CHECKS_JSON.
	if deployment != "" {
		fetchManagerHealthChecks(base, deployment, reg, client)
	}

	// Fetch config paths from manager (same as Java ManagerSyncTask when getOwnScriptPayloads is not empty)
	if scriptPayloads != nil && len(scriptPayloads) > 0 {
		configPathsURL := base + "/api/config-paths"
		req, err := http.NewRequest(http.MethodGet, configPathsURL, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err != nil {
					config.LogErrorf("Manager sync (config-paths): %v", err)
			} else {
				if resp.StatusCode == 200 {
					var fetchedPaths []string
					if json.NewDecoder(resp.Body).Decode(&fetchedPaths) == nil && len(fetchedPaths) > 0 {
						reg.SetPaths(fetchedPaths)
							config.LogDebugf("Updated config paths from manager: %v", fetchedPaths)
					}
				}
				resp.Body.Close()
			}
		}
	}

	// Fetch monitor pass/fail messages from manager settings (live-updates all agents without restart).
	fetchMonitorMessages(base, reg, client)

	// Fetch team assignments (deployment → team) so /monitor?team=X and /status?team=X filter correctly.
	fetchTeamAssignments(base, reg, client)

	// Fetch config exclusions from manager and apply immediately in registry.
	// Agent keeps only non-excluded files/targets/checks and removes excluded ones on every sync refresh.
	exclusionsURL := base + "/api/config-exclusions?deploymentName=" + url.QueryEscape(deployment)
	reqEx, errEx := http.NewRequest(http.MethodGet, exclusionsURL, nil)
	if errEx == nil {
		respEx, errEx := client.Do(reqEx)
		if errEx == nil {
			if respEx.StatusCode == 200 {
				var ex struct {
					Success bool `json:"success"`
					Items []map[string]interface{} `json:"items"`
				}
				if json.NewDecoder(respEx.Body).Decode(&ex) == nil && ex.Success {
					paths := make([]string, 0, len(ex.Items))
					for _, item := range ex.Items {
						if p, ok := item["path"].(string); ok && strings.TrimSpace(p) != "" {
							paths = append(paths, p)
						}
					}
					reg.SetExcludedPaths(paths)
				}
			}
			respEx.Body.Close()
		}
	}
}

// fetchManagerHealthChecks GETs /api/deployment-health-checks/yaml?deploymentName=... and stores active DB checks for the recurring health loop.
func fetchManagerHealthChecks(base, deployment string, reg *registry.Registry, client *http.Client) {
	yamlURL := base + "/api/deployment-health-checks/yaml?deploymentName=" + url.QueryEscape(deployment)
	req, err := http.NewRequest(http.MethodGet, yamlURL, nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
			config.LogErrorf("Manager sync (deployment-health-checks/yaml): %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	var result struct {
		Success bool   `json:"success"`
		JSON    string `json:"json"` // manager sends JSON string of checks array (double-encoded)
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.Success {
		return
	}
	var checks []models.JsonCheck
	if result.JSON == "" {
		reg.SetManagerHealthChecks(nil)
		return
	}
	if err := json.Unmarshal([]byte(result.JSON), &checks); err != nil {
			config.LogErrorf("Manager sync (deployment-health-checks/yaml): parse json: %v", err)
		return
	}
	reg.SetManagerHealthChecks(checks)
	if len(checks) > 0 {
			config.LogDebugf("Updated manager health checks from DB: %d checks for deployment %s", len(checks), deployment)
	}
}

// buildPendingTestDeployments returns deployment names to ask for pending tests: our deployment, cluster deployments, and ADDITIONAL_PENDING_TEST_DEPLOYMENTS (e.g. "help" for local dev).
func buildPendingTestDeployments(ourDeployment string, reg *registry.Registry, additionalEnv string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(d string) {
		if d == "" {
			return
		}
		if _, ok := seen[d]; ok {
			return
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	add(ourDeployment)
	co := reg.GetClusterObject()
	if co != nil && co.Deployments != nil {
		for d := range co.Deployments {
			add(d)
		}
	}
	for _, d := range strings.Split(additionalEnv, ",") {
		add(strings.TrimSpace(d))
	}
	return out
}

// fetchAndRunPendingTests GETs /api/pending-tests and for each test targeting our pod: file -> add path to registry; http/mysql -> run and POST /api/test-result (same as Java ManagerSyncTask).
func fetchAndRunPendingTests(base, deployment, myPodName string, reg *registry.Registry, client *http.Client) {
	pendingURL := base + "/api/pending-tests?deploymentName=" + url.QueryEscape(deployment)
	req, err := http.NewRequest(http.MethodGet, pendingURL, nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
			config.LogErrorf("Manager sync (pending-tests): %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	var result struct {
		Success bool                     `json:"success"`
		Tests   []map[string]interface{} `json:"tests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.Success || len(result.Tests) == 0 {
		return
	}
	containerName := reg.EffectiveContainerName()
	if containerName == "" {
		containerName = reg.EffectiveDeployment()
	}
	if containerName == "" {
		containerName = myPodName
	}
	for _, test := range result.Tests {
		testPod, _ := test["podName"].(string)
		testId, _ := test["testId"].(string)
		if testPod != myPodName {
			config.LogInfof("[manager-sync] pending test %s skipped: testPod=%q != myPod=%q (deployment=%s)", testId, testPod, myPodName, deployment)
			continue
		}
		if testId == "" {
			continue
		}
		kind, _ := test["kind"].(string)
		kind = strings.ToLower(strings.TrimSpace(kind))
		config.LogInfof("[manager-sync] running pending test %s kind=%s pod=%s deployment=%s", testId, kind, myPodName, deployment)
		switch kind {
		case "file":
			path, _ := test["path"].(string)
			mode := "ro"
			if m, ok := test["mode"].(string); ok && m != "" {
				// the UI sends "read"/"write"; the script and pass rules use "ro"/"rw"
				mode = models.NormalizeFileMode(m)
			}
			testContainer, _ := test["containerName"].(string)
			if testContainer == "" {
				testContainer = containerName
			}
			isExistingPath := false
			if b, ok := test["isExistingPath"].(bool); ok {
				isExistingPath = b
			}
			if path != "" {
				reg.AddFileAccessTarget(testPod, testContainer, models.FileAccessTarget{Path: path, Mode: mode})
				// Store path→testId so when script (agent.sh) posts script-callback with file result, agent can POST /api/test-result (QA + local).
				reg.AddPendingTestPath(testId, path, mode, testPod, testContainer, isExistingPath)
					config.LogDebugf("[manager-sync] Added pending file test path: %s (testId=%s)", path, testId)
			}
		case "http":
			urlVal, _ := test["url"].(string)
			timeoutMs := 5000
			if n, ok := test["timeoutMs"].(float64); ok && n > 0 {
				timeoutMs = int(n)
			}
		expectBody, _ := test["expectBodyContains"].(string)
		expectBodyNot, _ := test["expectBodyNotContains"].(string)
		httpUser, _ := test["user"].(string)
		httpPass, _ := test["pass"].(string)
		httpMethod, _ := test["httpMethod"].(string)
		res := health.RunHTTPPendingTestFull(urlVal, timeoutMs, expectBody, expectBodyNot, httpUser, httpPass, httpMethod)
			postTestResult(base, client, testId, res)
			if !res.Passed {
					config.LogWarnf("[manager-sync] HTTP pending test %s failed: duration=%dms", testId, res.Duration)
			} else {
					config.LogDebugf("[manager-sync] HTTP pending test %s completed: passed=%v, duration=%dms", testId, res.Passed, res.Duration)
			}
		case "mysql":
			host, _ := test["host"].(string)
			db, _ := test["db"].(string)
			user, _ := test["user"].(string)
			pass, _ := test["pass"].(string)
			res := health.RunMySQLPendingTest(host, db, user, pass)
			postTestResult(base, client, testId, res)
			if !res.Passed {
					config.LogWarnf("[manager-sync] MySQL pending test %s failed: duration=%dms", testId, res.Duration)
			} else {
					config.LogDebugf("[manager-sync] MySQL pending test %s completed: passed=%v, duration=%dms", testId, res.Passed, res.Duration)
			}
		case "redis":
			redisHost, _ := test["host"].(string)
			redisPass, _ := test["pass"].(string)
			redisTimeout := 5000
			if n, ok := test["timeoutMs"].(float64); ok && n > 0 {
				redisTimeout = int(n)
			}
			res := health.RunRedisPendingTest(redisHost, redisPass, redisTimeout)
			postTestResult(base, client, testId, res)
			if !res.Passed {
				config.LogWarnf("[manager-sync] Redis pending test %s failed: duration=%dms", testId, res.Duration)
			} else {
				config.LogDebugf("[manager-sync] Redis pending test %s completed: passed=%v, duration=%dms", testId, res.Passed, res.Duration)
			}
		case "postgres":
			pgHost, _ := test["host"].(string)
			pgTimeout := 5000
			if n, ok := test["timeoutMs"].(float64); ok && n > 0 {
				pgTimeout = int(n)
			}
			res := health.RunPostgresPendingTest(pgHost, pgTimeout)
			postTestResult(base, client, testId, res)
			if !res.Passed {
				config.LogWarnf("[manager-sync] PostgreSQL pending test %s failed: duration=%dms", testId, res.Duration)
			} else {
				config.LogDebugf("[manager-sync] PostgreSQL pending test %s completed: passed=%v, duration=%dms", testId, res.Passed, res.Duration)
			}
		case "mongodb":
			mgHost, _ := test["host"].(string)
			mgTimeout := 5000
			if n, ok := test["timeoutMs"].(float64); ok && n > 0 {
				mgTimeout = int(n)
			}
			res := health.RunMongoPendingTest(mgHost, mgTimeout)
			postTestResult(base, client, testId, res)
			if !res.Passed {
				config.LogWarnf("[manager-sync] MongoDB pending test %s failed: duration=%dms", testId, res.Duration)
			} else {
				config.LogDebugf("[manager-sync] MongoDB pending test %s completed: passed=%v, duration=%dms", testId, res.Passed, res.Duration)
			}
		case "kafka":
			kafkaHost, _ := test["host"].(string)
			kafkaTimeout := 5000
			if n, ok := test["timeoutMs"].(float64); ok && n > 0 {
				kafkaTimeout = int(n)
			}
			res := health.RunKafkaPendingTest(kafkaHost, kafkaTimeout)
			postTestResult(base, client, testId, res)
			if !res.Passed {
				config.LogWarnf("[manager-sync] Kafka pending test %s failed: duration=%dms", testId, res.Duration)
			} else {
				config.LogDebugf("[manager-sync] Kafka pending test %s completed: passed=%v, duration=%dms", testId, res.Passed, res.Duration)
			}
		case "action":
			// Fire an action URL from inside the pod — the result lets the UI confirm
			// the URL is reachable from the pod's network (not just from the manager).
			actionURL, _ := test["url"].(string)
			actionMethod, _ := test["actionMethod"].(string)
			authorization, _ := test["authorization"].(string)
			res := health.RunActionPendingTest(actionURL, actionMethod, authorization)
			postTestResult(base, client, testId, res)
			if !res.Passed {
				config.LogWarnf("[manager-sync] action pending test %s failed: url=%s duration=%dms msg=%s", testId, actionURL, res.Duration, res.Message)
			} else {
				config.LogDebugf("[manager-sync] action pending test %s completed: url=%s duration=%dms", testId, actionURL, res.Duration)
			}
		}
	}
}

func postTestResult(base string, client *http.Client, testId string, res health.PendingTestResult) {
	body := map[string]interface{}{
		"testId":   testId,
		"passed":   res.Passed,
		"message":  res.Message,
		"duration": res.Duration,
	}
	if len(res.Details) > 0 {
		body["details"] = res.Details
	}
	payload, _ := json.Marshal(body)
	// Retry once on failure so a transient manager error doesn't silently drop the result,
	// which would leave the UI spinning "WAITING FOR AGENT..." until the 5-min pending-test expiry.
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(http.MethodPost, base+"/api/test-result", bytes.NewReader(payload))
		if err != nil {
			config.LogErrorf("[manager-sync] test-result request build: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			config.LogWarnf("[manager-sync] POST test-result attempt %d: %v", attempt+1, err)
			if attempt == 0 {
				time.Sleep(2 * time.Second)
				continue
			}
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return
		}
		config.LogWarnf("[manager-sync] POST test-result attempt %d: HTTP %d", attempt+1, resp.StatusCode)
		if attempt == 0 {
			time.Sleep(2 * time.Second)
		}
	}
}

// filterHealthChecksByAllowed keeps only checks whose canonical key is in the manager's allowed set for this deployment.
// Allowed = all checks present in manager (DB + YAML, active or inactive). When allowed set is empty, no filtering (backward compat).
func filterHealthChecksByAllowed(healthChecks map[string]map[string][]models.HealthCheck, deployment string, reg *registry.Registry) map[string]map[string][]models.HealthCheck {
	if healthChecks == nil || deployment == "" {
		return healthChecks
	}
	allowedSet := reg.GetAllowedCheckKeysForDeployment(deployment)
	if len(allowedSet) == 0 {
		return healthChecks
	}
	out := make(map[string]map[string][]models.HealthCheck)
	for pod, contMap := range healthChecks {
		if contMap == nil {
			continue
		}
		outCont := make(map[string][]models.HealthCheck)
		for cont, list := range contMap {
			var kept []models.HealthCheck
			for _, h := range list {
				if CheckIsAllowed(h, allowedSet) {
					kept = append(kept, h)
				}
			}
			if len(kept) > 0 {
				outCont[cont] = kept
			}
		}
		if len(outCont) > 0 {
			out[pod] = outCont
		}
	}
	return out
}

// CheckIsAllowed returns true if the check's canonical key is in the allowed set (exported for server /status filtering).
// Auto-detected checks from config file parsing (haproxy.cfg, httpd.conf) have no Name; always allow them.
// Named MySQL and HTTP checks from YAML annotations are always allowed so they are not dropped when the manager's
// yamlChecks are empty (e.g. deployment lookup fails, wrong namespace, annotation key mismatch, or StatefulSet
// not handled by manager's Deployment-only lookup).
// Script-error checks (name starts with "script-", e.g. "No curl or wget found") have no canonical key in the manager; always allow so the message appears in the UI.
func CheckIsAllowed(h models.HealthCheck, allowedSet map[string]struct{}) bool {
	if strings.TrimSpace(h.Name) == "" {
		return true
	}
	name := strings.TrimSpace(h.Name)
	if strings.HasPrefix(name, "script-") {
		return true
	}
	t := strings.ToLower(strings.TrimSpace(h.Type))
	if (t == "mysql" || t == "http" || t == "redis" || t == "postgres" || t == "mongodb") && name != "" {
		return true
	}
	keys := checkCanonicalKeys(h)
	for _, k := range keys {
		if _, ok := allowedSet[k]; ok {
			return true
		}
	}
	return false
}

// checkCanonicalKeys returns keys that match manager's inactive key format (type::identifier, lowercase).
func checkCanonicalKeys(h models.HealthCheck) []string {
	t := strings.ToLower(strings.TrimSpace(h.Type))
	name := strings.ToLower(strings.TrimSpace(h.Name))
	var keys []string
	switch t {
	case "http":
		u := strings.TrimSpace(h.URL)
		if u == "" {
			return nil
		}
		u = strings.ToLower(u)
		keys = append(keys, "http::"+u, "http:"+name+":"+u)
		norm := strings.TrimPrefix(u, "https://")
		norm = strings.TrimPrefix(norm, "http://")
		for len(norm) > 1 && norm[len(norm)-1] == '/' {
			norm = norm[:len(norm)-1]
		}
		if norm != "" {
			keys = append(keys, "http::"+norm, "http:"+name+":"+norm)
			// Agent stores URL as base only (e.g. "http://localhost") so norm can be "localhost"; manager has "localhost:8080". Add port variants so either matches.
			if norm == "localhost" || norm == "127.0.0.1" {
				keys = append(keys, "http::"+norm+":8080", "http:"+name+":"+norm+":8080", "http::"+norm+":80", "http:"+name+":"+norm+":80")
			}
			// Manager may omit default port in path URLs (http://localhost/foo) while YAML uses :80 or :8080.
			if idx := strings.Index(norm, ":80/"); idx >= 0 {
				stripped := norm[:idx] + "/" + norm[idx+4:]
				keys = append(keys, "http::"+stripped, "http:"+name+":"+stripped)
			}
			if idx := strings.Index(norm, ":8080/"); idx >= 0 {
				stripped := norm[:idx] + "/" + norm[idx+len(":8080/"):]
				keys = append(keys, "http::"+stripped, "http:"+name+":"+stripped)
			}
		}
	case "mysql":
		// Agent HealthCheck uses URL for MySQL (e.g. host:3306 or scheme://host:3306); extract host.
		u := strings.TrimSpace(h.URL)
		if u == "" {
			return nil
		}
		host := u
		if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
			host = parsed.Host
		}
		host = strings.ToLower(host)
		keys = append(keys, "mysql::"+host, "mysql:"+name+":"+host)
		without3306 := strings.TrimSuffix(host, ":3306")
		keys = append(keys, "mysql::"+without3306, "mysql:"+name+":"+without3306)
	case "file":
		path := strings.TrimSpace(h.Path)
		if path == "" {
			return nil
		}
		path = strings.ToLower(path)
		keys = append(keys, "file::"+path, "file:"+name+":"+path)
	case "redis", "postgres", "mongodb", "kafka":
		host := strings.ToLower(strings.TrimSpace(h.URL))
		if host == "" {
			return nil
		}
		keys = append(keys, t+"::"+host, t+":"+name+":"+host)
	default:
		return nil
	}
	return keys
}

// buildInactiveCheckKeys builds canonical keys for checks where active == false (same as Java).
func buildInactiveCheckKeys(dbChecks []map[string]interface{}) []string {
	var keys []string
	for _, c := range dbChecks {
		if active, _ := c["active"].(bool); active {
			continue
		}
		keys = append(keys, buildCheckKeysFromMap(c)...)
	}
	return keys
}

// valueNestedField returns a field from the nested "value" map of a check (Kubernetes annotation format:
// {"kind":"http","value":{"url":"...","result":"..."}}). Returns "" if not found.
func valueNestedField(c map[string]interface{}, field string) string {
	vm, ok := c["value"].(map[string]interface{})
	if !ok {
		return ""
	}
	return refOrValue(vm[field])
}

// buildCheckKeysFromMap returns canonical keys for one check map (kind/name/url|path|host). Used for allowed set.
// Handles both flat DB format (url at top level) and nested Kubernetes annotation format (url under value.url).
func buildCheckKeysFromMap(c map[string]interface{}) []string {
	kind := strings.ToLower(refOrValueStr(c["kind"]))
	name := strings.ToLower(strings.TrimSpace(refOrValueStr(c["name"])))
	var keys []string
	switch kind {
	case "http":
		u := refOrValue(c["url"])
		if u == "" {
			// Kubernetes annotation format: {"kind":"http","value":{"url":"http://..."}}
			u = valueNestedField(c, "url")
		}
		if u == "" {
			return nil
		}
		u = strings.ToLower(strings.TrimSpace(u))
		keys = append(keys, "http:"+name+":"+u, "http::"+u)
		norm := strings.TrimPrefix(u, "https://")
		norm = strings.TrimPrefix(norm, "http://")
		for len(norm) > 1 && norm[len(norm)-1] == '/' {
			norm = norm[:len(norm)-1]
		}
		if norm != "" {
			keys = append(keys, "http:"+name+":"+norm, "http::"+norm)
			// So agent URL "http://localhost" (norm=localhost) matches manager "http://localhost:8080/" (norm=localhost:8080)
			if strings.HasSuffix(norm, ":8080") || strings.HasSuffix(norm, ":80") {
				hostOnly := strings.TrimSuffix(norm, ":8080")
				hostOnly = strings.TrimSuffix(hostOnly, ":80")
				if hostOnly != "" {
					keys = append(keys, "http:"+name+":"+hostOnly, "http::"+hostOnly)
				}
			}
			if idx := strings.Index(norm, ":80/"); idx >= 0 {
				stripped := norm[:idx] + "/" + norm[idx+4:]
				keys = append(keys, "http:"+name+":"+stripped, "http::"+stripped)
			}
			if idx := strings.Index(norm, ":8080/"); idx >= 0 {
				stripped := norm[:idx] + "/" + norm[idx+len(":8080/"):]
				keys = append(keys, "http:"+name+":"+stripped, "http::"+stripped)
			}
		}
	case "mysql":
		host := refOrValue(c["host"])
		if host == "" {
			host = refOrValue(c["url"])
			if parsed, err := url.Parse(host); err == nil && parsed.Host != "" {
				host = parsed.Host
			}
		}
		if host == "" {
			// Kubernetes annotation format: {"kind":"mysql","value":{"host":"..."}}
			host = valueNestedField(c, "host")
		}
		if host == "" {
			return nil
		}
		host = strings.ToLower(strings.TrimSpace(host))
		keys = append(keys, "mysql:"+name+":"+host, "mysql::"+host)
		without3306 := strings.TrimSuffix(host, ":3306")
		keys = append(keys, "mysql:"+name+":"+without3306, "mysql:"+name+":"+without3306+":3306",
			"mysql::"+without3306, "mysql::"+without3306+":3306")
	case "file":
		path := refOrValue(c["path"])
		if path == "" {
			// Kubernetes annotation format: {"kind":"file","value":{"path":"..."}}
			path = valueNestedField(c, "path")
		}
		if path == "" {
			return nil
		}
		path = strings.ToLower(strings.TrimSpace(path))
		keys = append(keys, "file:"+name+":"+path, "file::"+path)
	case "redis", "postgres", "mongodb", "kafka":
		host := refOrValue(c["host"])
		if host == "" {
			host = valueNestedField(c, "host")
		}
		if host == "" {
			return nil
		}
		host = strings.ToLower(strings.TrimSpace(host))
		keys = append(keys, kind+":"+name+":"+host, kind+"::"+host)
	default:
		return nil
	}
	return keys
}

// buildAllowedCheckKeys returns the set of canonical keys for all checks present in manager (DB + YAML).
func buildAllowedCheckKeys(dbChecks, yamlChecks []map[string]interface{}) map[string]struct{} {
	set := make(map[string]struct{})
	for _, c := range dbChecks {
		for _, k := range buildCheckKeysFromMap(c) {
			set[k] = struct{}{}
		}
	}
	for _, c := range yamlChecks {
		for _, k := range buildCheckKeysFromMap(c) {
			set[k] = struct{}{}
		}
	}
	return set
}

func refOrValue(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if m, ok := v.(map[string]interface{}); ok {
		if val, ok := m["value"].(string); ok && val != "" {
			return val
		}
		if fromEnv, ok := m["fromEnv"]; ok {
			return toString(fromEnv)
		}
	}
	return ""
}

func refOrValueStr(v interface{}) string {
	return toString(v)
}

func toString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// fetchMonitorMessages GETs /api/settings/monitor-messages and stores pass/fail messages in registry.
func fetchMonitorMessages(base string, reg *registry.Registry, client *http.Client) {
	req, err := http.NewRequest(http.MethodGet, base+"/api/settings/monitor-messages", nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		config.LogErrorf("Manager sync (monitor-messages): %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	var result struct {
		PassMessage string `json:"pass_message"`
		FailMessage string `json:"fail_message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		config.LogErrorf("Manager sync (monitor-messages): parse: %v", err)
		return
	}
	reg.SetMonitorMessages(result.PassMessage, result.FailMessage)
}

// fetchTeamAssignments GETs /api/teams/assignments and stores the deployment→team map and team→color map in registry.
func fetchTeamAssignments(base string, reg *registry.Registry, client *http.Client) {
	req, err := http.NewRequest(http.MethodGet, base+"/api/teams/assignments", nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		config.LogErrorf("Manager sync (team-assignments): %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	var result struct {
		Assignments map[string]string `json:"assignments"`
		Colors      map[string]string `json:"colors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		config.LogErrorf("Manager sync (team-assignments): parse: %v", err)
		return
	}
	reg.SetTeamAssignments(result.Assignments)
	reg.SetTeamColors(result.Colors)
}

// replayPayload sends a previously buffered AgentPayload to the manager.
// Failures are silently ignored — the entry is already removed from the buffer,
// so a replay failure just means that cycle stays missing rather than re-queuing.
// measureTCPRtt opens and immediately closes a TCP connection to the manager host.
// It returns the duration of the TCP 3-way handshake in milliseconds, which is pure
// network latency with zero server-side processing overhead.
// Returns 0 if the URL cannot be parsed or the connection fails.
func measureTCPRtt(managerBase string) int64 {
	u, err := url.Parse(managerBase)
	if err != nil || u.Host == "" {
		return 0
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(host, port)
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return 0
	}
	rtt := time.Since(start).Milliseconds()
	conn.Close()
	return rtt
}

func replayPayload(base string, p models.AgentPayload, client *http.Client) {
	callbackURL := base + "/api/agent-callback?deployment=" + url.QueryEscape(p.DeploymentName)
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, callbackURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		config.LogWarnf("Manager sync: replay failed for deployment=%s: %v", p.DeploymentName, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		config.LogWarnf("Manager sync: replay returned HTTP %d for deployment=%s", resp.StatusCode, p.DeploymentName)
	}
}
