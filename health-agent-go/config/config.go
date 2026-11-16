package config

import (
	"fmt"
	"log"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version is set at build time via -ldflags "-X config.Version=..." (same idea as Java agent.version in application.properties).
// Each image reports its own version; env AGENT_VERSION can still override.
var Version string

// Config holds agent configuration from env (same as Java AgentInit).
type Config struct {
	PodName       string
	Deployment    string
	Namespace     string
	PodIP         string
	ManagerURL    string
	ServerPort    int
	AgentVersion  string
	// PEER_AGENTS: comma-separated host or host:port for local agent-to-agent sync (e.g. "localhost:8082").
	PeerAgents string
	// PEER_AGENT_PORT: port for peer agents when using DNS discovery (default 8082).
	PeerAgentPort int
	// CONTAINER_NAME: app container name (e.g. from Kyverno when running as sidecar). Used for HC_CHECKS_JSON / status.
	ContainerName string
	// ADDITIONAL_PENDING_TEST_DEPLOYMENTS: comma-separated deployment names to also fetch pending tests for (e.g. "help" when running one agent locally for all tabs).
	AdditionalPendingTestDeployments string
}

// Load reads config with the same logic as Java AgentInit.init(): env first, then K8s file / hostname / host.
func Load() *Config {
	// Version: env override, else baked-in (ldflags), else default (same as Java agent.version from property file)
	agentVer := getEnv("AGENT_VERSION", Version)
	if agentVer == "" {
		agentVer = "0.0.1-SNAPSHOT"
	}
	defaultManager := "http://health-manager.infrastructure.svc.cluster.local:8080"
	managerURL := strings.TrimSpace(os.Getenv("HEALTH_AGENT_MANAGER_URL"))
	if managerURL == "" {
		managerURL = getEnv("MANAGER_URL", defaultManager)
	}
	c := &Config{
		ManagerURL:   managerURL,
		ServerPort:   listenPortFromEnv(),
		AgentVersion: agentVer,
	}

	// 1) Namespace: same as Java — env then /var/run/secrets/kubernetes.io/serviceaccount/namespace, else "default"
	c.Namespace = os.Getenv("NAMESPACE")
	if c.Namespace == "" {
		if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			c.Namespace = strings.TrimSpace(string(b))
		}
	}
	if c.Namespace == "" {
		c.Namespace = "default"
	}

	// 2) PodName: same as Java — env then hostname, else "localhost"
	c.PodName = os.Getenv("POD_NAME")
	if c.PodName == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			c.PodName = h
		}
	}
	if c.PodName == "" {
		c.PodName = "localhost"
	}

	// 3) PodIP: same as Java — env then local host address, else "127.0.0.1"
	c.PodIP = os.Getenv("POD_IP")
	if c.PodIP == "" {
		if ip := getOutboundIP(); ip != "" {
			c.PodIP = ip
		}
	}
	if c.PodIP == "" {
		c.PodIP = "127.0.0.1"
	}

	// 4) Deployment: same as Java — env then strip K8s suffix(es) from pod name.
	// Deployment pod:  <name>-<rs-hash(8-10 chars, contains digit)>-<pod-id(5 chars)>
	//   e.g. ai-service-backend-76b74b5b65-vlwv2
	// StatefulSet pod: <name>-<ordinal>
	//   e.g. web-site-0
	// DaemonSet pod:   <name>-<node-hash(5 alphanumeric chars)>
	//   e.g. edge-haproxy-9vp7q
	// The ReplicaSet hash always contains at least one digit; DaemonSet/deployment-name
	// segments that are pure letters (e.g. "haproxy") must NOT be treated as hashes.
	c.Deployment = os.Getenv("DEPLOYMENT")
	if c.Deployment == "" && c.PodName != "" {
		// Deployment: strip <rs-hash with digit>-<5-char pod-id>
		reDeploy := regexp.MustCompile(`-[a-z0-9]*[0-9][a-z0-9]*-[a-z0-9]{5}$`)
		c.Deployment = reDeploy.ReplaceAllString(c.PodName, "")
		if c.Deployment == c.PodName {
			// StatefulSet: strip trailing ordinal number
			reStatefulSet := regexp.MustCompile(`-\d+$`)
			c.Deployment = reStatefulSet.ReplaceAllString(c.PodName, "")
		}
		if c.Deployment == c.PodName {
			// DaemonSet: strip single 5-char alphanumeric node-hash suffix
			reDaemonSet := regexp.MustCompile(`-[a-z0-9]{5}$`)
			c.Deployment = reDaemonSet.ReplaceAllString(c.PodName, "")
		}
	}

	c.PeerAgents = os.Getenv("PEER_AGENTS")
	c.ContainerName = getEnv("CONTAINER", "")
	c.AdditionalPendingTestDeployments = os.Getenv("ADDITIONAL_PENDING_TEST_DEPLOYMENTS")
	if c.ContainerName == "" {
		c.ContainerName = os.Getenv("CONTAINER_NAME")
	}
	if p := os.Getenv("PEER_AGENT_PORT"); p != "" {
		if port, err := strconv.Atoi(p); err == nil && port > 0 {
			c.PeerAgentPort = port
		}
	}
	if c.PeerAgentPort == 0 {
		c.PeerAgentPort = 8082
	}
	for len(c.ManagerURL) > 0 && c.ManagerURL[len(c.ManagerURL)-1] == '/' {
		c.ManagerURL = c.ManagerURL[:len(c.ManagerURL)-1]
	}
	return c
}

// listenPortFromEnv prefers HEALTH_AGENT_LISTEN_PORT so a mirrored app SERVER_PORT (e.g. Node on 80) cannot override the sidecar when HEALTH_AGENT_LISTEN_PORT is set.
func listenPortFromEnv() int {
	if p := strings.TrimSpace(os.Getenv("HEALTH_AGENT_LISTEN_PORT")); p != "" {
		if port, err := strconv.Atoi(p); err == nil && port > 0 {
			return port
		}
	}
	if p := strings.TrimSpace(os.Getenv("SERVER_PORT")); p != "" {
		if port, err := strconv.Atoi(p); err == nil && port > 0 {
			return port
		}
	}
	return 8082
}

// getOutboundIP returns a non-loopback local IP (same idea as Java InetAddress.getLocalHost().getHostAddress()).
func getOutboundIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return ""
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// --- Leveled agent logging (stderr). HEALTH_AGENT_LOG_LEVEL: error | warn | info | debug (default warn).
// Kept in this file so Docker/CI always compile it with the rest of package config.

type agentLogLevel int

const (
	agentLogLevelError agentLogLevel = 1
	agentLogLevelWarn  agentLogLevel = 2
	agentLogLevelInfo  agentLogLevel = 3
	agentLogLevelDebug agentLogLevel = 4
)

var currentAgentLogLevel = agentLogLevelWarn

func init() {
	currentAgentLogLevel = parseAgentLogLevel(os.Getenv("HEALTH_AGENT_LOG_LEVEL"))
}

func parseAgentLogLevel(s string) agentLogLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error", "err", "critical":
		return agentLogLevelError
	case "warn", "warning":
		return agentLogLevelWarn
	case "info":
		return agentLogLevelInfo
	case "debug", "verbose", "trace":
		return agentLogLevelDebug
	case "":
		return agentLogLevelWarn
	default:
		return agentLogLevelWarn
	}
}

func agentLogEnabled(min agentLogLevel) bool { return currentAgentLogLevel >= min }

// --- Optional local fail log (append-only, inside agent container). See WriteFailLogf.

var (
	failLogMu   sync.Mutex
	failLogOnce sync.Once
	failLogFile *os.File
	failLogOff  bool
)

// failLogOpen runs once: opens path from HEALTH_AGENT_FAIL_LOG (default /tmp/health-agent-failures.log),
// or sets failLogOff when HEALTH_AGENT_FAIL_LOG=off (or -, none, false, 0) or open fails.
func failLogOpen() {
	p := strings.TrimSpace(os.Getenv("HEALTH_AGENT_FAIL_LOG"))
	if p == "" {
		p = "/tmp/health-agent-failures.log"
	}
	switch strings.ToLower(p) {
	case "-", "off", "none", "false", "0":
		failLogOff = true
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		failLogOff = true
		return
	}
	failLogFile = f
}

// WriteFailLogf appends one UTC timestamped line to the fail log (when enabled). Safe for concurrent use.
// Used for ERROR/WARN and for health check failures independent of HEALTH_AGENT_LOG_LEVEL.
func WriteFailLogf(format string, args ...interface{}) {
	failLogOnce.Do(failLogOpen)
	if failLogOff || failLogFile == nil {
		return
	}
	line := fmt.Sprintf("%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
	failLogMu.Lock()
	defer failLogMu.Unlock()
	_, _ = failLogFile.WriteString(line)
}

// LogErrorf logs operational failures.
func LogErrorf(format string, args ...interface{}) {
	if agentLogEnabled(agentLogLevelError) {
		log.Printf(format, args...)
	}
	WriteFailLogf("ERROR "+format, args...)
}

// LogWarnf logs issues that are not hard errors.
func LogWarnf(format string, args ...interface{}) {
	if agentLogEnabled(agentLogLevelWarn) {
		log.Printf(format, args...)
	}
	WriteFailLogf("WARN "+format, args...)
}

// LogInfof logs startup and infrequent lifecycle.
func LogInfof(format string, args ...interface{}) {
	if agentLogEnabled(agentLogLevelInfo) {
		log.Printf(format, args...)
	}
}

// LogDebugf logs high-volume trace (sync payloads, per-check lines, ScriptConfig JSON).
func LogDebugf(format string, args ...interface{}) {
	if agentLogEnabled(agentLogLevelDebug) {
		log.Printf(format, args...)
	}
}
