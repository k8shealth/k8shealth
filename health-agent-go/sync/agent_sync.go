package sync

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

// RunAgentSync runs the agent-to-agent sync: POST own payload to each peer. Sleep interval respects manager settings (updated when manager sync fetches).
func RunAgentSync(cfg *config.Config, reg *registry.Registry, intervals *Intervals) {
	if intervals == nil {
		intervals = NewIntervals()
	}
	client := &http.Client{Timeout: 10 * time.Second}
	for {
		runAgentOnce(cfg, reg, client)
		time.Sleep(intervals.GetAgentInterval())
	}
}

func runAgentOnce(cfg *config.Config, reg *registry.Registry, client *http.Client) {
	ownScript := reg.GetOwnScriptPayloads()
	if len(ownScript) == 0 {
			config.LogDebugf("Waiting for agent script to register...")
		return
	}
	payload := models.AgentPayload{
		PodName:            reg.EffectivePodName(),
		DeploymentName:     reg.EffectiveDeployment(),
		MemoryUsageBytes:   AgentProcessMemorySys(),
		ScriptPayloads:     reg.GetOwnScriptPayloads(),
		HealthBaseChecks:   reg.GetOwnHealthBaseChecks(),
		ClusterObject:      reg.GetClusterObject(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
			config.LogErrorf("Agent sync: marshal: %v", err)
		return
	}
		config.LogDebugf("AgentSync: payload %d bytes", len(body))
	peerURLs := getPeerSyncURLs(cfg)
	for _, u := range peerURLs {
		req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
			config.LogDebugf("Syncing with peer: %s", u)
		resp, err := client.Do(req)
		if err != nil {
				config.LogErrorf("Agent sync to %s: %v", u, err)
			continue
		}
		resp.Body.Close()
	}
}

// getPeerSyncURLs returns full sync URLs (http://host:port/api/sync) for each peer.
// If PEER_AGENTS is set (e.g. "localhost:8082"), uses those as host:port and builds URLs.
// Otherwise resolves health-agents.{namespace}.svc.cluster.local and uses PEER_AGENT_PORT (default 8082).
func getPeerSyncURLs(cfg *config.Config) []string {
	if v := cfg.PeerAgents; v != "" {
		port := 8082
		if cfg.PeerAgentPort > 0 {
			port = cfg.PeerAgentPort
		}
		var urls []string
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if strings.Contains(s, ":") {
				urls = append(urls, "http://"+s+"/api/sync")
			} else {
				urls = append(urls, "http://"+s+":"+strconv.Itoa(port)+"/api/sync")
			}
		}
		return urls
	}
	// DNS-based peer discovery (K8s)
	port := 8082
	if cfg.PeerAgentPort > 0 {
		port = cfg.PeerAgentPort
	}
	dns := "health-agents." + cfg.Namespace + ".svc.cluster.local"
	ips, err := net.LookupIP(dns)
	if err != nil {
		// No health-agents Service in this namespace — peer DNS discovery not configured. Not an error.
		config.LogDebugf("Agent sync: resolve %s: %v", dns, err)
		return nil
	}
	self := cfg.PodIP
	if self == "" {
		self = "127.0.0.1"
	}
	var urls []string
	for _, ip := range ips {
		ipStr := ip.String()
		if ipStr == self {
			continue
		}
		if ip.To4() != nil {
			urls = append(urls, "http://"+ipStr+":"+strconv.Itoa(port)+"/api/sync")
		}
	}
	return urls
}
