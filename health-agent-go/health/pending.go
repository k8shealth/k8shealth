package health

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// PendingTestResult holds the result of a single pending test run (HTTP or MySQL).
type PendingTestResult struct {
	Passed   bool
	Message  string
	Duration int
	Details  map[string]interface{}
}

// RunHTTPPendingTest runs one HTTP request and returns result. Does not add to registry.
func RunHTTPPendingTest(rawURL string, timeoutMs int, expectBodyContains string, user, pass string) PendingTestResult {
	return RunHTTPPendingTestFull(rawURL, timeoutMs, expectBodyContains, "", user, pass, "")
}

// RunHTTPPendingTestFull runs one HTTP request (method defaults to GET), checking both contains and not-contains constraints.
func RunHTTPPendingTestFull(rawURL string, timeoutMs int, expectBodyContains, expectBodyNotContains string, user, pass string, method string) PendingTestResult {
	out := PendingTestResult{Details: make(map[string]interface{})}
	if rawURL == "" {
		out.Message = "URL is required"
		out.Details["error"] = "Missing url"
		return out
	}
	urlStr := rawURL
	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
		urlStr = "http://" + urlStr
	}
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	client := &http.Client{Timeout: time.Duration(timeoutMs) * time.Millisecond}
	httpMethod := strings.ToUpper(strings.TrimSpace(method))
	if httpMethod == "" {
		httpMethod = http.MethodGet
	}
	req, reqErr := http.NewRequest(httpMethod, urlStr, nil)
	if reqErr != nil {
		out.Message = "Invalid URL: " + reqErr.Error()
		out.Details["error"] = reqErr.Error()
		return out
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	start := time.Now()
	resp, err := client.Do(req)
	out.Duration = int(time.Since(start).Milliseconds())
	var status int
	var body string
	if err != nil {
		out.Message = "Connection timeout or network error: " + err.Error()
		out.Details["error"] = err.Error()
		return out
	}
	defer func() { _ = resp.Body.Close() }()
	status = resp.StatusCode
	if httpMethod != http.MethodHead {
		// bounded: a huge response must not exhaust the sidecar's memory limit
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		body = string(bodyBytes)
	}
	bodyLower := strings.ToLower(body)
	out.Passed = status >= 200 && status < 300
	if httpMethod == http.MethodHead {
		// HEAD has no body — skip body match checks; just report status
		if out.Passed {
			out.Message = "HTTP HEAD request successful"
		} else {
			out.Message = "HTTP HEAD request failed with status " + itoa(status)
		}
	} else if !out.Passed {
		// A non-2xx status is the real failure: report it first. Checking the body of an error page against
		// the expected text only produces a misleading "does not contain expected text" message.
		out.Message = "HTTP request failed with status " + itoa(status) + " " + http.StatusText(status)
		if user != "" && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
			out.Message += " - the server rejected the Basic Auth credentials that were sent; remove them if this endpoint is not protected"
		} else if user == "" && status == http.StatusUnauthorized {
			out.Message += " - the endpoint requires authentication; add Basic Auth credentials"
		}
	} else if expectBodyContains != "" && !strings.Contains(bodyLower, strings.ToLower(expectBodyContains)) {
		out.Passed = false
		out.Message = "Response body does not contain expected text: \"" + expectBodyContains + "\""
	} else if expectBodyNotContains != "" && strings.Contains(bodyLower, strings.ToLower(expectBodyNotContains)) {
		out.Passed = false
		out.Message = "Response body must NOT contain: \"" + expectBodyNotContains + "\""
	} else if out.Passed {
		out.Message = "HTTP request successful"
	} else {
		out.Message = "HTTP request failed with status " + itoa(status)
	}
	out.Details["statusCode"] = status
	if len(body) > 500 {
		out.Details["responseBody"] = body[:500]
	} else {
		out.Details["responseBody"] = body
	}
	return out
}

// RunMySQLPendingTest runs one MySQL connection test and returns result (same as Java runMysqlTestAndPostResult). Does not add to registry.
func RunMySQLPendingTest(host, db, user, pass string) PendingTestResult {
	out := PendingTestResult{Details: make(map[string]interface{})}
	if host == "" {
		out.Message = "Host is required"
		out.Details["error"] = "Missing host"
		return out
	}
	start := time.Now()
	// Build DSN: host may be "host:port"
	hostPort := host
	if !strings.Contains(hostPort, ":") {
		hostPort = hostPort + ":3306"
	}
	dsn, err := buildMySQLDSN(user, pass, hostPort, db, 5*time.Second, 5*time.Second)
	var dbConn *sql.DB
	if err == nil {
		dbConn, err = sql.Open("mysql", dsn)
	}
	out.Duration = int(time.Since(start).Milliseconds())
	if err != nil {
		out.Message = "MySQL connection error: " + err.Error()
		out.Details["error"] = err.Error()
		return out
	}
	defer dbConn.Close()
	dbConn.SetConnMaxLifetime(5 * time.Second)
	err = dbConn.Ping()
	if err != nil {
		out.Message = "MySQL connection error: " + err.Error()
		out.Details["error"] = err.Error()
		return out
	}
	out.Passed = true
	out.Message = "MySQL connection successful"
	return out
}

// RunRedisPendingTest runs a Redis PING/PONG test via raw TCP. Does not add to registry.
func RunRedisPendingTest(host, pass string, timeoutMs int) PendingTestResult {
	out := PendingTestResult{Details: make(map[string]interface{})}
	if host == "" {
		out.Message = "Host is required"
		out.Details["error"] = "Missing host"
		return out
	}
	hostPort := host
	if !strings.Contains(hostPort, ":") {
		hostPort = hostPort + ":6379"
	}
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond
	start := time.Now()
	conn, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		out.Duration = int(time.Since(start).Milliseconds())
		out.Message = "Redis connection error: " + err.Error()
		out.Details["error"] = err.Error()
		return out
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	// AUTH if password provided
	if pass != "" {
		fmt.Fprintf(conn, "*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(pass), pass)
		authResp, authErr := bufio.NewReader(conn).ReadString('\n')
		if authErr != nil || !strings.HasPrefix(strings.TrimSpace(authResp), "+OK") {
			out.Duration = int(time.Since(start).Milliseconds())
			resp := strings.TrimSpace(authResp)
			if authErr != nil {
				resp = authErr.Error()
			}
			out.Message = "Redis AUTH failed: " + resp
			out.Details["error"] = "AUTH failed: " + resp
			return out
		}
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	// Send PING
	fmt.Fprintf(conn, "PING\r\n")
	pongResp, pongErr := bufio.NewReader(conn).ReadString('\n')
	out.Duration = int(time.Since(start).Milliseconds())
	if pongErr != nil {
		out.Message = "Redis PING error: " + pongErr.Error()
		out.Details["error"] = pongErr.Error()
		return out
	}
	pong := strings.TrimSpace(pongResp)
	if pong == "+PONG" {
		out.Passed = true
		out.Message = "Redis PING successful (+PONG)"
	} else {
		out.Message = "Redis unexpected response: " + pong
		out.Details["error"] = "Expected +PONG, got: " + pong
	}
	return out
}

// RunPostgresPendingTest checks PostgreSQL by sending an SSLRequest startup packet.
func RunPostgresPendingTest(host string, timeoutMs int) PendingTestResult {
	out := PendingTestResult{Details: make(map[string]interface{})}
	if host == "" {
		out.Message = "Host is required"
		out.Details["error"] = "Missing host"
		return out
	}
	hostPort := host
	if !strings.Contains(hostPort, ":") {
		hostPort = hostPort + ":5432"
	}
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond
	start := time.Now()
	conn, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		out.Duration = int(time.Since(start).Milliseconds())
		out.Message = "PostgreSQL connection error: " + err.Error()
		out.Details["error"] = err.Error()
		return out
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	pkt := make([]byte, 8)
	binary.BigEndian.PutUint32(pkt[0:4], 8)
	binary.BigEndian.PutUint32(pkt[4:8], 80877103)
	conn.Write(pkt)
	resp := make([]byte, 1)
	_, readErr := io.ReadFull(conn, resp)
	out.Duration = int(time.Since(start).Milliseconds())
	if readErr != nil {
		out.Message = "Read response: " + readErr.Error()
		out.Details["error"] = readErr.Error()
		return out
	}
	switch resp[0] {
	case 'S', 'N', 'E':
		out.Passed = true
		out.Message = "PostgreSQL connection successful"
	default:
		out.Message = fmt.Sprintf("Unexpected server byte: 0x%02x", resp[0])
		out.Details["error"] = out.Message
	}
	return out
}

// RunMongoPendingTest checks MongoDB by sending an OP_MSG isMaster command.
func RunMongoPendingTest(host string, timeoutMs int) PendingTestResult {
	out := PendingTestResult{Details: make(map[string]interface{})}
	if host == "" {
		out.Message = "Host is required"
		out.Details["error"] = "Missing host"
		return out
	}
	hostPort := strings.TrimPrefix(host, "mongodb://")
	if i := strings.Index(hostPort, "/"); i >= 0 {
		hostPort = hostPort[:i]
	}
	if !strings.Contains(hostPort, ":") {
		hostPort = hostPort + ":27017"
	}
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond
	start := time.Now()
	conn, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		out.Duration = int(time.Since(start).Milliseconds())
		out.Message = "MongoDB connection error: " + err.Error()
		out.Details["error"] = err.Error()
		return out
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	conn.Write(mongoIsMasterMsg())
	hdr := make([]byte, 4)
	_, readErr := io.ReadFull(conn, hdr)
	out.Duration = int(time.Since(start).Milliseconds())
	if readErr != nil {
		out.Message = "Read response: " + readErr.Error()
		out.Details["error"] = readErr.Error()
		return out
	}
	respLen := binary.LittleEndian.Uint32(hdr)
	if respLen >= 4 && respLen <= 65536 {
		out.Passed = true
		out.Message = "MongoDB connection successful"
	} else {
		out.Message = fmt.Sprintf("Unexpected response length: %d", respLen)
		out.Details["error"] = out.Message
	}
	return out
}


// RunKafkaPendingTest checks a Kafka broker by sending an ApiVersions handshake (API key 18, version 0).
// Same protocol as runKafkaCheck — no auth required. Does not add to registry.
func RunKafkaPendingTest(host string, timeoutMs int) PendingTestResult {
	out := PendingTestResult{Details: make(map[string]interface{})}
	if host == "" {
		out.Message = "Host is required"
		out.Details["error"] = "Missing host"
		return out
	}
	hostPort := host
	if !strings.Contains(hostPort, ":") {
		hostPort = hostPort + ":9092"
	}
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond
	start := time.Now()
	conn, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		out.Duration = int(time.Since(start).Milliseconds())
		out.Message = "Kafka connection error: " + err.Error()
		out.Details["error"] = err.Error()
		return out
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	pkt := make([]byte, 14)
	binary.BigEndian.PutUint32(pkt[0:4], 10)
	binary.BigEndian.PutUint16(pkt[4:6], 18)
	binary.BigEndian.PutUint16(pkt[6:8], 0)
	binary.BigEndian.PutUint32(pkt[8:12], 1)
	binary.BigEndian.PutUint16(pkt[12:14], 0xffff)
	if _, err = conn.Write(pkt); err != nil {
		out.Duration = int(time.Since(start).Milliseconds())
		out.Message = "Kafka write error: " + err.Error()
		out.Details["error"] = err.Error()
		return out
	}
	resp := make([]byte, 10)
	_, readErr := io.ReadFull(conn, resp)
	out.Duration = int(time.Since(start).Milliseconds())
	if readErr != nil {
		out.Message = "Kafka read error: " + readErr.Error()
		out.Details["error"] = readErr.Error()
		return out
	}
	respLen := binary.BigEndian.Uint32(resp[0:4])
	errorCode := int16(binary.BigEndian.Uint16(resp[8:10]))
	if respLen >= 6 && respLen <= 65536 {
		out.Passed = true
		if errorCode == 0 {
			out.Message = "Kafka broker connection successful"
		} else {
			out.Message = fmt.Sprintf("Kafka broker connection successful (errorCode=%d)", errorCode)
		}
	} else {
		out.Message = fmt.Sprintf("Unexpected response length: %d", respLen)
		out.Details["error"] = out.Message
	}
	return out
}

// RunActionPendingTest fires an action URL from inside the pod (pending-test variant of FireCheckAction).
// Queued via kind="action" so the HTTP call originates from the agent pod network, not the manager.
func RunActionPendingTest(actionURL, method, authorization string) PendingTestResult {
	out := PendingTestResult{Details: make(map[string]interface{})}
	if actionURL == "" {
		out.Message = "Action URL is required"
		out.Details["error"] = out.Message
		return out
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = "POST"
	}
	reqBody := []byte(`{"event":"action.test","message":"Test invoke from Health Manager UI"}`)
	req, err := http.NewRequest(method, actionURL, bytes.NewReader(reqBody))
	if err != nil {
		out.Message = "Request build error: " + err.Error()
		out.Details["error"] = out.Message
		return out
	}
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	start := time.Now()
	client := newActionClient()
	resp, err := client.Do(req)
	out.Duration = int(time.Since(start).Milliseconds())
	if err != nil {
		out.Message = "Request error: " + err.Error()
		out.Details["error"] = out.Message
		return out
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	shown := body
	if len(shown) > 500 {
		shown = shown[:500]
	}
	out.Details["statusCode"] = resp.StatusCode
	out.Details["responseBody"] = string(shown)
	ok, reason := actionResponseOK(resp.StatusCode, body)
	out.Passed = ok
	if ok {
		out.Message = "HTTP " + strconv.Itoa(resp.StatusCode)
	} else {
		out.Message = reason
	}
	return out
}
