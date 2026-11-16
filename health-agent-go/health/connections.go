package health

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/models"
)

const (
	tcpStateEstablished = "01"
	connScanInterval    = time.Second
)

// connEntry tracks when a connection was first and last seen.
type connEntry struct {
	firstSeen time.Time
	lastSeen  time.Time
}

// ConnectionScanner reads /proc/net/tcp and /proc/net/tcp6 every second and
// accumulates a rolling-window union of all observed TCP connections.
// Filtering (port skip list, inbound detection) is now done server-side by the manager.
// No elevated privileges required — reads only the pod's own network namespace.
type ConnectionScanner struct {
	mu          sync.RWMutex
	connections map[connKey]*connEntry // key = (remote "ip:port", local port)
	software    *SoftwareScanner       // optional; see software_detect.go
}

// NewConnectionScanner returns a new ConnectionScanner.
func NewConnectionScanner() *ConnectionScanner {
	return &ConnectionScanner{
		connections: make(map[connKey]*connEntry),
	}
}

// Run starts the 1-second scan loop. Call in a goroutine.
func (s *ConnectionScanner) Run() {
	for {
		s.scan()
		time.Sleep(connScanInterval)
	}
}

// ephemeralLocalPortFrom is the lowest local port treated as an ephemeral (client-side) port.
// Linux hands out 32768-60999 by default.
const ephemeralLocalPortFrom = 30000

// normalizeLocalPort collapses ephemeral local ports to 0.
//
// For an OUTBOUND connection the local port is a random ephemeral port that changes with every new
// connection. Using it in the map key meant each new connection to the same dependency (for example
// a DB or an HTTP API behind a pool or keep-alive churn) created a new entry that was kept for the
// whole 24 h TTL, and the whole set was copied and serialised on every manager callback. On busy pods
// that grew into tens of thousands of entries and showed up as steadily rising agent RAM.
//
// The manager only uses the local port to recognise INBOUND connections (a low server port,
// below conn_inbound_local_max, default 10000). An ephemeral local port is never inbound, and 0 is
// treated as "not inbound" too, so collapsing it does not change any classification.
func normalizeLocalPort(p int) int {
	if p >= ephemeralLocalPortFrom {
		return 0
	}
	return p
}

// connKey combines remote ip:port and local port into a map key.
type connKey struct {
	remote    string // "ip:port"
	localPort int
}

// scan reads the proc tcp files and updates the union set.
// Filtering (port skip list, inbound detection) is now done server-side by the manager,
// so the agent sends raw connections.  Only loopback / link-local IPs are still dropped
// here because they can never be valid external dependencies regardless of policy.
func (s *ConnectionScanner) scan() {
	now := time.Now()
	seen := make(map[connKey]struct{})

	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if err != nil {
			continue // not available on Windows/local dev — silently skip
		}
		scanner := bufio.NewScanner(f)
		scanner.Scan() // skip header line
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 4 {
				continue
			}
			if fields[3] != tcpStateEstablished {
				continue // only ESTABLISHED connections
			}
			isIPv6 := strings.HasSuffix(path, "tcp6")
			// Decode remote address (the dependency this pod connects to).
			ip, remotePort, err := decodeHexAddr(fields[2], isIPv6)
			if err != nil || skipIP(ip) {
				continue
			}
			// Pre-filter ephemeral / NodePort range before accumulating in the map.
			// Inbound connections from Kubernetes clients have ephemeral source ports
			// (>= 32768 on Linux), which the manager also discards server-side via
			// conn_skip_port_range_from (default 30000).  Accumulating them agent-side
			// for 24 h caused OOM on high-traffic pods (e.g. mobile-notifications during
			// calendar events).  Only well-known service ports (3306, 6379, 443, 8080 …)
			// are relevant for dependency insight — those are always < 30000.
			if skipFrom := config.AgentSettings.GetConnSkipPortRangeFrom(); skipFrom > 0 && remotePort >= skipFrom {
				continue
			}
			// Decode local port — sent to manager so it can classify inbound vs. outbound.
			_, localPort, localErr := decodeHexAddr(fields[1], isIPv6)
			if localErr != nil {
				localPort = 0
			}
			localPort = normalizeLocalPort(localPort)
			seen[connKey{remote: fmt.Sprintf("%s:%d", ip, remotePort), localPort: localPort}] = struct{}{}
		}
		f.Close()
	}

	// Configurable TTL — read on every scan tick so manager changes take effect without restart.
	ttlHours := config.AgentSettings.GetConnTtlHours()
	if ttlHours <= 0 {
		ttlHours = 24
	}
	connTTL := time.Duration(ttlHours) * time.Hour

	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range seen {
		if e, ok := s.connections[k]; ok {
			e.lastSeen = now
		} else {
			s.connections[k] = &connEntry{firstSeen: now, lastSeen: now}
		}
	}
	// Prune entries not seen within the rolling TTL window.
	cutoff := now.Add(-connTTL)
	for k, e := range s.connections {
		if e.lastSeen.Before(cutoff) {
			delete(s.connections, k)
		}
	}
}

// GetConnections returns the current rolling-window union set as a slice.
func (s *ConnectionScanner) GetConnections() []models.DetectedConnection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.connections) == 0 {
		return nil
	}
	out := make([]models.DetectedConnection, 0, len(s.connections))
	for k, e := range s.connections {
		colon := strings.LastIndex(k.remote, ":")
		if colon < 0 {
			continue
		}
		remotePort, _ := strconv.Atoi(k.remote[colon+1:])
		out = append(out, models.DetectedConnection{
			RemoteIP:   k.remote[:colon],
			RemotePort: remotePort,
			LocalPort:  k.localPort,
			FirstSeen:  e.firstSeen.Unix(),
			LastSeen:   e.lastSeen.Unix(),
		})
	}
	return out
}

// decodeHexAddr decodes a /proc/net/tcp hex address "XXXXXXXX:XXXX" to IP and port.
// Both IPv4 (/proc/net/tcp) and IPv6 (/proc/net/tcp6) are stored little-endian per 4-byte group.
func decodeHexAddr(hexAddr string, isIPv6 bool) (string, int, error) {
	parts := strings.SplitN(hexAddr, ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("invalid addr: %s", hexAddr)
	}
	port, err := strconv.ParseInt(parts[1], 16, 32)
	if err != nil {
		return "", 0, err
	}

	hexIP := parts[0]
	if isIPv6 {
		if len(hexIP) != 32 {
			return "", 0, fmt.Errorf("invalid ipv6 len: %d", len(hexIP))
		}
		// Decode 4 groups of 4 bytes, each group stored little-endian
		result := make([]byte, 16)
		for i := 0; i < 4; i++ {
			b, err := hex.DecodeString(hexIP[i*8 : (i+1)*8])
			if err != nil {
				return "", 0, err
			}
			// Reverse bytes (little-endian → big-endian)
			result[i*4], result[i*4+1], result[i*4+2], result[i*4+3] = b[3], b[2], b[1], b[0]
		}
		ip6 := net.IP(result)
		if ip4 := ip6.To4(); ip4 != nil {
			return ip4.String(), int(port), nil
		}
		return ip6.String(), int(port), nil
	}

	// IPv4: 8-char hex, little-endian
	if len(hexIP) != 8 {
		return "", 0, fmt.Errorf("invalid ipv4 len: %d", len(hexIP))
	}
	b, err := hex.DecodeString(hexIP)
	if err != nil {
		return "", 0, err
	}
	// Bytes are little-endian: reverse to get dotted-decimal
	return fmt.Sprintf("%d.%d.%d.%d", b[3], b[2], b[1], b[0]), int(port), nil
}

// skipIP returns true for IPs we should never report (loopback, unspecified, link-local).
func skipIP(ip string) bool {
	if ip == "" {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return true
	}
	return parsed.IsLoopback() || parsed.IsUnspecified() || parsed.IsLinkLocalUnicast()
}

// NOTE: skipPort() and isInboundConnection() have been removed from the agent.
// Port filtering and inbound-vs-outbound classification are now done server-side
// by the manager (DependencyInsightService.filterConnections) using configurable
// settings from manager_config.  The agent sends raw connections with LocalPort
// so the manager can perform the classification without needing a rebuilt agent.
// Old agents (without LocalPort) are handled gracefully: manager skips inbound
// detection for connections where localPort == 0 (treats them as outbound).
