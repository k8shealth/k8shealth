package health

import (
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

// responderBodyBytes is how much of an error page is read to recognise the layer that produced it.
const responderBodyBytes = 2048

// shortHTTPResponseMessage maps errors to a short message (same style as Java, which leaves response empty for script-callback HTTP checks).
func shortHTTPResponseMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return "Timeout"
	}
	if strings.Contains(msg, "connection refused") {
		return "Connection refused"
	}
	return "Connection failed"
}

// DoHTTPCheck runs a single HTTP GET for rawURL and adds the result to the registry (used when processing script payload).
func DoHTTPCheck(pod, container, rawURL string, reg *registry.Registry) {
	h, ok := BuildScriptHTTPCheck(rawURL)
	if !ok {
		return
	}
	reg.AddHealthCheck(pod, container, "http", h)
}

// BuildScriptHTTPCheck runs script-style HTTP check and returns a health check result.
func BuildScriptHTTPCheck(rawURL string) (models.HealthCheck, bool) {
	if rawURL == "" {
		return models.HealthCheck{}, false
	}
	urlStr := rawURL
	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
		urlStr = "http://" + urlStr
	}
	scriptTimeoutMs := config.AgentSettings.GetScriptHttpTimeoutMs()
	if scriptTimeoutMs <= 0 {
		scriptTimeoutMs = 2000
	}
	client := &http.Client{Timeout: time.Duration(scriptTimeoutMs) * time.Millisecond}
	start := time.Now()
	resp, err := client.Get(urlStr)
	duration := int(time.Since(start).Milliseconds())
	var status int
	var body string
	var responder string
	if err != nil {
		body = shortHTTPResponseMessage(err)
		status = 0
	} else {
		status = resp.StatusCode
		if resp.Body != nil {
			if status >= 400 {
				snip, _ := io.ReadAll(io.LimitReader(resp.Body, responderBodyBytes))
				responder = ClassifyResponder(resp.Header, string(snip))
			}
			resp.Body.Close()
		}
	}
	passed := status >= 200 && status < 300 && duration < 2000
	_, port := baseURLAndPort(urlStr)
	return models.HealthCheck{
		Type:         "http",
		Passed:       passed,
		Duration:     duration,
		Time:         time.Now(),
		URL:          urlStr,
		Port:         port,
		Response:     body,
		ResponseCode: itoa(status),
		Responder:    responder,
	}, true
}

func runHTTPCheck(pod, container string, c ResolvedCheck, reg *registry.Registry) bool {
	url := c.URL
	if url == "" {
		return false
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}
	if c.Path != "" {
		url = strings.TrimRight(url, "/") + "/" + strings.TrimLeft(c.Path, "/")
	}
	timeoutMs := c.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	client := &http.Client{Timeout: time.Duration(timeoutMs) * time.Millisecond}
	method := strings.ToUpper(strings.TrimSpace(c.Method))
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		reg.AddHealthCheck(pod, container, "http", models.HealthCheck{
			Type: "http", Name: c.Name, Passed: false,
			Time: time.Now(), URL: url, Response: "Invalid URL",
		})
		return false
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Pass)
	}
	start := time.Now()
	resp, err := client.Do(req)
	duration := int(time.Since(start).Milliseconds())
	var status int
	var body string
	var classBody string // first bytes of an error page, used only to name the responding layer
	if err != nil {
		body = shortHTTPResponseMessage(err)
		status = 0
	} else {
		status = resp.StatusCode
		if resp.Body != nil {
			if method != http.MethodHead && (c.Contains != "" || c.NotContains != "") {
				// Read up to configured limit (default 200 KB) for contains/not-contains checks.
				bodyReadKb := config.AgentSettings.GetCheckBodyReadKb()
				if bodyReadKb <= 0 {
					bodyReadKb = 200
				}
				bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, int64(bodyReadKb)*1024))
				body = html.UnescapeString(string(bodyBytes))
				classBody = body
				if len(classBody) > responderBodyBytes {
					classBody = classBody[:responderBodyBytes]
				}
			} else if method != http.MethodHead && status >= 400 {
				snip, _ := io.ReadAll(io.LimitReader(resp.Body, responderBodyBytes))
				classBody = string(snip)
			}
			resp.Body.Close()
		}
	}

	// Status code check: exact match when statusCode is set, otherwise any 2xx
	var statusPassed bool
	if c.StatusCode != "" {
		statusPassed = itoa(status) == c.StatusCode
	} else {
		statusPassed = status >= 200 && status < 300
	}

	// Body contains check (case-insensitive, HTML entities decoded on both sides)
	containsPassed := true
	if c.Contains != "" {
		containsPassed = strings.Contains(strings.ToLower(body), strings.ToLower(html.UnescapeString(c.Contains)))
	}

	// Body not-contains check: fails if the string IS found
	notContainsPassed := true
	if c.NotContains != "" {
		notContainsPassed = !strings.Contains(strings.ToLower(body), strings.ToLower(html.UnescapeString(c.NotContains)))
	}

	passed := statusPassed && containsPassed && notContainsPassed

	// ExpectBody surface: prefix with "!" for not-contains so the manager UI can show it correctly
	expectBodyDisplay := c.Contains
	if c.NotContains != "" {
		expectBodyDisplay = "!" + c.NotContains
	}

	// Name the layer that answered with an error (nginx / HAProxy / Cloudflare ...) so the incident shows it.
	responder := ""
	if err == nil && !passed && status >= 400 {
		responder = ClassifyResponder(resp.Header, classBody)
	}

	_, port := baseURLAndPort(url)
	h := models.HealthCheck{
		Type:           "http",
		Name:           c.Name,
		Responder:      responder,
		Passed:         passed,
		Duration:       duration,
		Time:           time.Now(),
		CheckStartedAt: start,
		URL:            url, // full URL incl. path (same as DoHTTPCheck); base-only loses path in manager UI
		Port:           port,
		Response:       body,
		ResponseCode:   itoa(status),
		ExpectBody:     expectBodyDisplay,
		TimeoutMs:      timeoutMs,
		HTTPMethod:     method,
	}
	reg.AddHealthCheck(pod, container, "http", h)
	return passed
}

func baseURLAndPort(raw string) (base string, port int) {
	parsed, err := url.Parse(raw)
	if err != nil {
		if strings.HasPrefix(raw, "https://") {
			return raw, 443
		}
		return raw, 80
	}
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "http"
	}
	host := parsed.Hostname()
	if host == "" {
		host = parsed.Host
	}
	base = scheme + "://" + host
	port = 80
	if scheme == "https" {
		port = 443
	}
	if p := parsed.Port(); p != "" {
		port = atoi(p)
	}
	return base, port
}

func itoa(i int) string {
	if i <= 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func atoi(s string) int {
	var n int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}
