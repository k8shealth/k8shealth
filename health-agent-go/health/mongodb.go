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

// mongoIsMasterMsg returns a hard-coded 55-byte OP_MSG with {"isMaster":1,"$db":"admin"}
func mongoIsMasterMsg() []byte {
	// BSON: {"isMaster":1,"$db":"admin"} = 34 bytes
	bson := []byte{
		0x22, 0x00, 0x00, 0x00, // doc length = 34
		0x10,                                           // int32 type
		0x69, 0x73, 0x4d, 0x61, 0x73, 0x74, 0x65, 0x72, 0x00, // "isMaster\0"
		0x01, 0x00, 0x00, 0x00, // value = 1
		0x02,                   // string type
		0x24, 0x64, 0x62, 0x00, // "$db\0"
		0x06, 0x00, 0x00, 0x00, // string length = 6
		0x61, 0x64, 0x6d, 0x69, 0x6e, 0x00, // "admin\0"
		0x00, // document end
	}
	// OP_MSG = header(16) + flagBits(4) + sectionKind(1) + bson(34) = 55
	msg := make([]byte, 55)
	binary.LittleEndian.PutUint32(msg[0:4], 55)    // messageLength
	binary.LittleEndian.PutUint32(msg[4:8], 1)     // requestID
	binary.LittleEndian.PutUint32(msg[8:12], 0)    // responseTo
	binary.LittleEndian.PutUint32(msg[12:16], 2013) // opCode OP_MSG
	binary.LittleEndian.PutUint32(msg[16:20], 0)   // flagBits
	msg[20] = 0                                     // section kind 0
	copy(msg[21:], bson)
	return msg
}

// runMongoCheck connects to MongoDB and sends a minimal OP_MSG isMaster command.
// A valid response length confirms the server is alive — no auth required.
func runMongoCheck(pod, container string, c ResolvedCheck, reg *registry.Registry) bool {
	hostPort := c.Host
	if hostPort == "" {
		hostPort = c.URL
	}
	if hostPort == "" {
		reg.AddHealthCheck(pod, container, "mongodb", models.HealthCheck{
			Type: "mongodb", Name: c.Name, Passed: false, Time: time.Now(),
			Response: "No host configured",
		})
		return false
	}
	// Strip mongodb:// scheme if present
	hostPort = strings.TrimPrefix(hostPort, "mongodb://")
	if i := strings.Index(hostPort, "/"); i >= 0 {
		hostPort = hostPort[:i]
	}
	if !strings.Contains(hostPort, ":") {
		hostPort = hostPort + ":27017"
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
		if _, err = conn.Write(mongoIsMasterMsg()); err != nil {
			message = "Write: " + err.Error()
		} else {
			// Read 4-byte response length
			hdr := make([]byte, 4)
			if _, err = io.ReadFull(conn, hdr); err != nil {
				message = "Read header: " + err.Error()
			} else {
				respLen := binary.LittleEndian.Uint32(hdr)
				if respLen >= 4 && respLen <= 65536 {
					passed = true
					message = "MongoDB connection successful"
				} else {
					message = fmt.Sprintf("Unexpected response length: %d", respLen)
				}
			}
		}
	}
	duration := int(time.Since(start).Milliseconds())
	h := models.HealthCheck{
		Type: "mongodb", Name: c.Name, Passed: passed,
		Duration: duration, Time: time.Now(), CheckStartedAt: start,
		Host: hostPort, Response: message,
	}
	reg.AddHealthCheck(pod, container, "mongodb", h)
	return passed
}
