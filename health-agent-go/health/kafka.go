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

// runKafkaCheck connects to a Kafka broker and sends an ApiVersions request (API key 18, version 0).
// This is the standard broker greeting — no auth required, works on any Kafka version >= 0.10.
// A non-zero error code in the response still means the broker is alive and responding.
func runKafkaCheck(pod, container string, c ResolvedCheck, reg *registry.Registry) bool {
	hostPort := c.Host
	if hostPort == "" {
		hostPort = c.URL
	}
	if hostPort == "" {
		reg.AddHealthCheck(pod, container, "kafka", models.HealthCheck{
			Type: "kafka", Name: c.Name, Passed: false, Time: time.Now(),
			Response: "No host configured",
		})
		return false
	}
	if !strings.Contains(hostPort, ":") {
		hostPort = hostPort + ":9092"
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
		// ApiVersions request: remainingLen(4) + apiKey(2) + apiVersion(2) + correlationId(4) + clientId null(-1, 2 bytes)
		// Total frame = INT32 remainingLen=10 then the 10-byte payload.
		pkt := make([]byte, 14)
		binary.BigEndian.PutUint32(pkt[0:4], 10)      // remaining length
		binary.BigEndian.PutUint16(pkt[4:6], 18)      // apiKey = ApiVersions
		binary.BigEndian.PutUint16(pkt[6:8], 0)       // apiVersion = 0
		binary.BigEndian.PutUint32(pkt[8:12], 1)      // correlationId = 1
		binary.BigEndian.PutUint16(pkt[12:14], 0xffff) // clientId null (len = -1)
		if _, err = conn.Write(pkt); err != nil {
			message = "Write: " + err.Error()
		} else {
			// Response: INT32 remainingLen + INT32 correlationId + INT16 errorCode + ...
			// Read 10 bytes (4+4+2) to get the error code.
			resp := make([]byte, 10)
			if _, err = io.ReadFull(conn, resp); err != nil {
				message = "Read: " + err.Error()
			} else {
				respLen := binary.BigEndian.Uint32(resp[0:4])
				errorCode := int16(binary.BigEndian.Uint16(resp[8:10]))
				if respLen >= 6 && respLen <= 65536 && errorCode == 0 {
					passed = true
					message = "Kafka broker connection successful"
				} else if respLen >= 6 && respLen <= 65536 {
					// Non-zero error code — broker alive but returned an error (e.g. unsupported version).
					// Still consider alive; log the code.
					passed = true
					message = fmt.Sprintf("Kafka broker connection successful (errorCode=%d)", errorCode)
				} else {
					message = fmt.Sprintf("Unexpected response length: %d", respLen)
				}
			}
		}
	}
	duration := int(time.Since(start).Milliseconds())
	h := models.HealthCheck{
		Type: "kafka", Name: c.Name, Passed: passed,
		Duration: duration, Time: time.Now(), CheckStartedAt: start,
		Host: hostPort, Response: message,
	}
	reg.AddHealthCheck(pod, container, "kafka", h)
	return passed
}
