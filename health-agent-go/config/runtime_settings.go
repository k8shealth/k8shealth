package config

import "sync"

// RuntimeSettings holds agent policy settings fetched from the manager's
// /api/settings/sync-intervals endpoint. Updated by sync/intervals.go after every
// successful fetch; read by health and connections packages.
//
// All fields default to the values that were previously hardcoded, so the agent
// behaves identically before the first manager response arrives.
type RuntimeSettings struct {
	mu sync.RWMutex

	// Connection filtering — previously hardcoded in health/connections.go.
	// These are now evaluated in the manager when it receives DetectedConnections,
	// but the agent also sends LocalPort so the manager can apply inbound detection.
	ConnSkipPorts         []int // individual ports never reported as dependencies
	ConnSkipPortRangeFrom int   // skip ports >= this value (NodePort / ephemeral range)
	ConnTtlHours          int   // 24-hour rolling window kept by the scanner

	// HTTP check settings — previously hardcoded in health/http.go.
	ScriptHttpTimeoutMs int // timeout for script-callback (health-agent.sh) HTTP checks
	CheckBodyReadKb     int // max response body bytes read for contains/not-contains checks

	// Runner settings — previously hardcoded in health/runner.go.
	CheckFailRetrySecs int // fast-retry interval when a check is failing
}

// AgentSettings is the process-wide singleton updated by sync.Intervals.Fetch().
var AgentSettings = &RuntimeSettings{
	ConnSkipPorts:         []int{53, 2379, 2380, 6443, 10250, 10255, 10256, 8082},
	ConnSkipPortRangeFrom: 30000,
	ConnTtlHours:          1, // 1 h default — manager's own TTL is 30 min; 24 h caused OOM on high-traffic pods
	ScriptHttpTimeoutMs:   2000,
	CheckBodyReadKb:       200,
	CheckFailRetrySecs:    5,
}

// Update atomically replaces all settings. Called by sync/intervals.go after a
// successful fetch from the manager.
func (s *RuntimeSettings) Update(
	skipPorts []int,
	skipPortRangeFrom int,
	ttlHours int,
	scriptHttpTimeoutMs int,
	checkBodyReadKb int,
	checkFailRetrySecs int,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(skipPorts) > 0 {
		s.ConnSkipPorts = skipPorts
	}
	if skipPortRangeFrom > 0 {
		s.ConnSkipPortRangeFrom = skipPortRangeFrom
	}
	if ttlHours > 0 {
		s.ConnTtlHours = ttlHours
	}
	if scriptHttpTimeoutMs > 0 {
		s.ScriptHttpTimeoutMs = scriptHttpTimeoutMs
	}
	if checkBodyReadKb > 0 {
		s.CheckBodyReadKb = checkBodyReadKb
	}
	if checkFailRetrySecs > 0 {
		s.CheckFailRetrySecs = checkFailRetrySecs
	}
}

// GetConnTtlHours returns the configurable connection TTL (safe for concurrent use).
func (s *RuntimeSettings) GetConnTtlHours() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ConnTtlHours
}

// GetConnSkipPortRangeFrom returns the ephemeral-port threshold (safe for concurrent use).
// Connections whose remotePort >= this value are pre-filtered in the agent before being
// accumulated in the scanner map so high-traffic pods don't OOM on inbound ephemeral ports.
func (s *RuntimeSettings) GetConnSkipPortRangeFrom() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ConnSkipPortRangeFrom
}

// GetScriptHttpTimeoutMs returns the configurable script HTTP timeout (safe for concurrent use).
func (s *RuntimeSettings) GetScriptHttpTimeoutMs() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ScriptHttpTimeoutMs
}

// GetCheckBodyReadKb returns the configurable body read cap in KB (safe for concurrent use).
func (s *RuntimeSettings) GetCheckBodyReadKb() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.CheckBodyReadKb
}

// GetCheckFailRetrySecs returns the configurable fail-retry interval in seconds (safe for concurrent use).
func (s *RuntimeSettings) GetCheckFailRetrySecs() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.CheckFailRetrySecs
}
