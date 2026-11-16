package health

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// newActionClient returns the HTTP client used for action calls. Redirects are NOT followed:
// Go turns a redirected POST (301/302/303) into a GET and would report the final page's 200
// (e.g. a login page), which made failing actions look successful.
func newActionClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// actionResponseOK decides whether an action call really succeeded: HTTP 2xx and, when the body is a
// JSON object, no app-level failure flag ({"success":false} / {"ok":false}). Many services answer
// errors with a 2xx status and put the failure only in the body.
func actionResponseOK(status int, body []byte) (bool, string) {
	if status < 200 || status >= 300 {
		reason := "HTTP " + strconv.Itoa(status)
		if status >= 300 && status < 400 {
			reason += " (redirect not followed)"
		}
		if m := bodyMessage(body); m != "" {
			reason += ": " + m
		}
		return false, reason
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var obj map[string]interface{}
		if json.Unmarshal(trimmed, &obj) == nil {
			for _, k := range []string{"success", "ok"} {
				if v, has := obj[k].(bool); has && !v {
					reason := "HTTP " + strconv.Itoa(status) + " but response reports failure"
					if m := bodyMessage(body); m != "" {
						reason += ": " + m
					}
					return false, reason
				}
			}
		}
	}
	return true, ""
}

// bodyMessage extracts a short message from a JSON error body, else the raw text (max 160 chars).
func bodyMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var obj map[string]interface{}
	if json.Unmarshal([]byte(trimmed), &obj) == nil {
		for _, k := range []string{"message", "error", "detail"} {
			if s, ok := obj[k].(string); ok && s != "" {
				trimmed = s
				break
			}
		}
	}
	if len(trimmed) > 160 {
		trimmed = trimmed[:160]
	}
	return trimmed
}
