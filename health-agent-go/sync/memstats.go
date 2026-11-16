package sync

import "runtime"

// AgentProcessMemorySys returns runtime.MemStats.HeapInuse: live heap pages currently in use (bytes).
// HeapInuse is the accurate measure of agent memory consumption. The old metric (Sys) reported
// OS-reserved memory which Go never eagerly returns, inflating the displayed value by 3-5x.
// Sent on each manager callback so health-manager can show agent RAM on /agents.json without Kubernetes metrics.
func AgentProcessMemorySys() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapInuse)
}
