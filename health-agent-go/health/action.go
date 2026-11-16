package health

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

// actionPayload is the JSON body sent to the action URL.
type actionPayload struct {
	CheckName string `json:"checkName"`
	Status    string `json:"status"`   // "failing" or "recovered"
	Passed    bool   `json:"passed"`
	Kind      string `json:"kind"`
	Timestamp string `json:"timestamp"` // RFC3339
}

// FireCheckAction fires the action URL for a check state transition asynchronously.
// open=true means the check just started failing; open=false means it just recovered.
// Does nothing if the check has no ActionUrl configured.
// If open=false and ActionOnClose=false the call is also skipped.
func FireCheckAction(c ResolvedCheck, open bool, reg *registry.Registry) {
	if c.ActionUrl == "" {
		return
	}
	if !open && !c.ActionOnClose {
		return
	}
	// Fire in background so it never blocks the check loop.
	go fireActionAsync(c, open, reg)
}

func fireActionAsync(c ResolvedCheck, open bool, reg *registry.Registry) {
	status := "failing"
	event := "open"
	if !open {
		status = "recovered"
		event = "close"
	}
	payload := actionPayload{
		CheckName: c.Name,
		Status:    status,
		Passed:    !open,
		Kind:      c.Kind,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		config.LogErrorf("action fire marshal error for check %q: %v", c.Name, err)
		return
	}

	method := strings.ToUpper(strings.TrimSpace(c.ActionMethod))
	if method == "" {
		method = "POST"
	}

	start := time.Now()
	entry := models.ActionLogEntry{
		DeploymentName: c.DeploymentName,
		CheckName:      c.Name,
		Event:          event,
		FiredAt:        start.UnixMilli(),
	}

	req, err := http.NewRequest(method, c.ActionUrl, bytes.NewReader(body))
	if err != nil {
		entry.ErrorMessage = err.Error()
		entry.DurationMs = int(time.Since(start).Milliseconds())
		if reg != nil {
			reg.AppendActionLog(entry)
		}
		config.LogErrorf("action fire request build error for check %q url=%q: %v", c.Name, c.ActionUrl, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.ActionHeaders {
		req.Header.Set(k, v)
	}

	client := newActionClient()
	resp, err := client.Do(req)
	entry.DurationMs = int(time.Since(start).Milliseconds())
	if err != nil {
		entry.ErrorMessage = err.Error()
		if reg != nil {
			reg.AppendActionLog(entry)
		}
		config.LogErrorf("action fire error for check %q url=%q status=%s: %v", c.Name, c.ActionUrl, status, err)
		return
	}
	defer resp.Body.Close()

	entry.HttpStatus = resp.StatusCode

	// Read up to 2KB: used for the snippet and for app-level failure detection.
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	ok, reason := actionResponseOK(resp.StatusCode, respBytes)
	entry.Success = ok
	if !ok {
		entry.ErrorMessage = reason
	}
	snippet := respBytes
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	if len(snippet) > 0 {
		entry.ResponseSnippet = string(snippet)
	}

	if reg != nil {
		reg.AppendActionLog(entry)
	}
	if !ok {
		config.LogWarnf("action FAILED for check %q status=%s url=%q: %s (%dms)", c.Name, status, c.ActionUrl, reason, entry.DurationMs)
		return
	}
	config.LogDebugf("action fired for check %q status=%s -> HTTP %d (%dms)", c.Name, status, resp.StatusCode, entry.DurationMs)
}
