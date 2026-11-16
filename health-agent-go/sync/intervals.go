package sync

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fx/health-agent-go/config"
)

const (
	minSyncIntervalMs = 1000
	maxSyncIntervalMs = 300000
	defaultManagerMs  = 30000
	defaultAgentMs    = 20000
)

// Intervals holds sync intervals (ms) fetched from manager settings. Same as Java SyncIntervalConfig.
// Manager sync fetches GET /api/settings/sync-intervals and updates these; both manager and agent sync use them.
type Intervals struct {
	mu         sync.RWMutex
	AgentMs    int
	ManagerMs  int
}

// NewIntervals returns default intervals (used until first fetch from manager).
func NewIntervals() *Intervals {
	return &Intervals{
		AgentMs:   defaultAgentMs,
		ManagerMs: defaultManagerMs,
	}
}

func clampMs(ms int) int {
	if ms < minSyncIntervalMs {
		return minSyncIntervalMs
	}
	if ms > maxSyncIntervalMs {
		return maxSyncIntervalMs
	}
	return ms
}

// Fetch GETs manager baseURL + /api/settings/sync-intervals and updates intervals (same as Java ManagerSyncTask).
func (i *Intervals) Fetch(baseURL string, client *http.Client) {
	if baseURL == "" || client == nil {
		return
	}
	recover := func(prev string) {
		config.LogInfof("Manager sync (sync-intervals): OK (recovered, was %s)", prev)
	}
	url := baseURL + "/api/settings/sync-intervals"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		noteManagerSyncOutcomeString("sync-intervals", "builderr", recover,
			func() { config.LogErrorf("Manager sync (sync-intervals): build request: %v", err) })
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		noteManagerSyncOutcomeString("sync-intervals", "neterr", recover,
			func() { config.LogErrorf("Manager sync (sync-intervals): %v", err) })
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cur := managerHTTPOutcomeSig(nil, resp)
		// Same as historical behavior: do not log non-200 for this endpoint; still track state for recovery INFO.
		noteManagerSyncOutcomeString("sync-intervals", cur, recover, nil)
		return
	}
	var m map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		noteManagerSyncOutcomeString("sync-intervals", "decodeerr", recover,
			func() { config.LogErrorf("Manager sync (sync-intervals): decode: %v", err) })
		return
	}
	i.mu.Lock()
	if v, ok := m["agent_sync_interval_ms"]; ok {
		if n, ok := numInt(v); ok {
			i.AgentMs = clampMs(n)
		}
	}
	if v, ok := m["manager_sync_interval_ms"]; ok {
		if n, ok := numInt(v); ok {
			i.ManagerMs = clampMs(n)
		}
	}
	i.mu.Unlock()
	config.LogDebugf("Sync intervals from manager: agent=%dms manager=%dms", i.AgentMs, i.ManagerMs)

	// Parse agent policy settings pushed by manager and update the process-wide singleton.
	skipPorts := parseIntList(strVal(m["conn_skip_ports"]))
	skipRangeFrom, _ := numInt(m["conn_skip_port_range_from"])
	ttlHours, _ := numInt(m["conn_ttl_hours"])
	scriptTimeoutMs, _ := numInt(m["script_http_timeout_ms"])
	bodyReadKb, _ := numInt(m["check_body_read_kb"])
	failRetrySecs, _ := numInt(m["check_fail_retry_secs"])
	config.AgentSettings.Update(skipPorts, skipRangeFrom, ttlHours, scriptTimeoutMs, bodyReadKb, failRetrySecs)
	config.LogDebugf("Agent policy settings from manager: skipPorts=%v skipRangeFrom=%d ttlHours=%d scriptTimeoutMs=%d bodyReadKb=%d failRetrySecs=%d",
		skipPorts, skipRangeFrom, ttlHours, scriptTimeoutMs, bodyReadKb, failRetrySecs)
	// Log recovery only after a successful read; note may no-op when outcome unchanged.
	noteManagerSyncOutcomeString("sync-intervals", "ok", recover, nil)
}

func numInt(v interface{}) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	default:
		return 0, false
	}
}

// strVal coerces an interface{} to a string (for JSON string values from manager response).
func strVal(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// parseIntList splits a comma-separated string of port numbers into []int.
// Returns nil (not an empty slice) when the input is blank.
func parseIntList(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// GetManagerInterval returns duration for manager sync sleep (same as Java getManagerSyncIntervalMs).
func (i *Intervals) GetManagerInterval() time.Duration {
	i.mu.RLock()
	ms := i.ManagerMs
	i.mu.RUnlock()
	return time.Duration(ms) * time.Millisecond
}

// GetAgentInterval returns duration for agent sync sleep (same as Java getAgentSyncIntervalMs).
func (i *Intervals) GetAgentInterval() time.Duration {
	i.mu.RLock()
	ms := i.AgentMs
	i.mu.RUnlock()
	return time.Duration(ms) * time.Millisecond
}
