package health

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

// runRedisCheck connects to a Redis server via raw TCP, optionally authenticates,
// sends PING and expects a +PONG reply.
func runRedisCheck(pod, container string, c ResolvedCheck, reg *registry.Registry) bool {
	// Resolve host:port — default port 6379.
	hostPort := c.Host
	if hostPort == "" {
		hostPort = c.URL
	}
	if hostPort == "" {
		reg.AddHealthCheck(pod, container, "redis", models.HealthCheck{
			Type: "redis", Name: c.Name, Passed: false, Time: time.Now(),
			Response: "No host configured",
		})
		return false
	}
	if !strings.Contains(hostPort, ":") {
		hostPort = hostPort + ":6379"
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
		writer := bufio.NewWriter(conn)
		reader := bufio.NewReader(conn)

		// AUTH if password is set.
		if c.Pass != "" {
			cmd := fmt.Sprintf("*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(c.Pass), c.Pass)
			if _, err = writer.WriteString(cmd); err == nil {
				err = writer.Flush()
			}
			if err != nil {
				message = "AUTH write: " + err.Error()
			} else {
				line, readErr := reader.ReadString('\n')
				if readErr != nil {
					message = "AUTH read: " + readErr.Error()
					err = readErr
				} else if !strings.HasPrefix(line, "+OK") {
					message = "AUTH failed: " + strings.TrimSpace(line)
					err = fmt.Errorf("auth failed")
				}
			}
		}

		if err == nil {
			// Send PING using inline command.
			if _, err = writer.WriteString("PING\r\n"); err == nil {
				err = writer.Flush()
			}
			if err != nil {
				message = "PING write: " + err.Error()
			} else {
				line, readErr := reader.ReadString('\n')
				if readErr != nil {
					message = "PING read: " + readErr.Error()
				} else {
					trimmed := strings.TrimSpace(line)
					if trimmed == "+PONG" {
						passed = true
						message = "Redis connection successful"
					} else {
						message = "Unexpected response: " + trimmed
					}
				}
			}
		}
	}

	duration := int(time.Since(start).Milliseconds())
	h := models.HealthCheck{
		Type:           "redis",
		Name:           c.Name,
		Passed:         passed,
		Duration:       duration,
		Time:           time.Now(),
		CheckStartedAt: start,
		Host:           hostPort,
		Response:       message,
	}
	reg.AddHealthCheck(pod, container, "redis", h)
	return passed
}
