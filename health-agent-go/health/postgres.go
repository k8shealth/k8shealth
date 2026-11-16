package health

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

// runPostgresCheck connects to PostgreSQL, sends an SSLRequest startup packet,
// and expects any valid server response ('S', 'N', or error byte 'E').
// This mimics what pg_isready does — no credentials required.
func runPostgresCheck(pod, container string, c ResolvedCheck, reg *registry.Registry) bool {
	hostPort := c.Host
	if hostPort == "" {
		hostPort = c.URL
	}
	if hostPort == "" {
		reg.AddHealthCheck(pod, container, "postgres", models.HealthCheck{
			Type: "postgres", Name: c.Name, Passed: false, Time: time.Now(),
			Response: "No host configured",
		})
		return false
	}
	if !strings.Contains(hostPort, ":") {
		hostPort = hostPort + ":5432"
	}
	start := time.Now()
	timeoutDur := 5 * time.Second
	if c.TimeoutMs > 0 {
		timeoutDur = time.Duration(c.TimeoutMs) * time.Millisecond
	}
	conn, err := net.DialTimeout("tcp", hostPort, timeoutDur)
	var passed bool
	var message string
	if err != nil {
		message = "Connect: " + err.Error()
	} else {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(timeoutDur))
		// Send SSLRequest (8-byte startup: length=8, code=80877103)
		pkt := make([]byte, 8)
		binary.BigEndian.PutUint32(pkt[0:4], 8)
		binary.BigEndian.PutUint32(pkt[4:8], 80877103)
		if _, err = conn.Write(pkt); err != nil {
			message = "Write: " + err.Error()
		} else {
			resp := make([]byte, 1)
			if _, err = io.ReadFull(conn, resp); err != nil {
				message = "Read: " + err.Error()
			} else {
				switch resp[0] {
				case 'S', 'N': // SSL supported / not supported — server is alive
					passed = true
					message = "PostgreSQL connection successful"
				case 'E': // ErrorResponse — still a live server
					passed = true
					message = "PostgreSQL connection successful (TLS error response)"
				default:
					message = fmt.Sprintf("Unexpected server byte: 0x%02x", resp[0])
				}
			}
		}
	}
	duration := int(time.Since(start).Milliseconds())
	h := models.HealthCheck{
		Type: "postgres", Name: c.Name, Passed: passed,
		Duration: duration, Time: time.Now(), CheckStartedAt: start,
		Host: hostPort, Response: message,
	}
	reg.AddHealthCheck(pod, container, "postgres", h)
	return passed
}
