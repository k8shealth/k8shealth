package health

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/fx/health-agent-go/models"
)

// ResolvedCheck is a single health check with resolved values (url, user, pass, host, db, path).
type ResolvedCheck struct {
	Name       string
	Kind       string
	URL        string
	Method     string // HTTP method: GET (default), HEAD, POST
	User       string
	Pass       string
	Host       string
	Db         string
	Path       string
	Mode       string
	// Contains is the body substring to match (case-insensitive). Supersedes Result when both are set.
	Contains   string
	// NotContains: fail if the response body contains this string (case-insensitive).
	NotContains string
	// StatusCode is the exact HTTP status code expected (e.g. "200", "403"). Empty = any 2xx.
	StatusCode string
	// TimeoutMs is the HTTP request timeout in milliseconds. 0 means use the default (5000ms).
	TimeoutMs    int
	// IntervalSecs overrides the global 5s check cadence. 0 = use global default. Min 5s.
	IntervalSecs int
	// Action fields — fired by the agent from within the pod when the check changes state.
	ActionUrl         string
	ActionMethod      string
	ActionHeaders     map[string]string
	ActionWaitForIncident bool // true = fire on first fail (default); false = fire after 2 consecutive fails
	ActionOnClose     bool
	// DeploymentName is set by the runner so action log entries can be correlated to the deployment.
	DeploymentName string
	// FromDB is true when this check was fetched from the manager DB (via fetchManagerHealthChecks),
	// as opposed to being parsed from a HC_CHECKS_JSON pod annotation.
	// Used in Pass 2 dedup to distinguish DB→DB displacement (alias, don't prune) from
	// YAML→DB displacement (prune the stale YAML entry from registry).
	FromDB bool
}

// LoadFromEnv reads HEALTH_AGENT_HC_CHECKS_JSON or legacy HC_CHECKS_JSON from env and returns resolved checks.
func LoadFromEnv(env map[string]string) ([]ResolvedCheck, error) {
	jsonStr := env["HEALTH_AGENT_HC_CHECKS_JSON"]
	if jsonStr == "" {
		jsonStr = env["HC_CHECKS_JSON"]
	}
	if jsonStr == "" {
		return nil, nil
	}
	var list []models.JsonCheck
	if err := json.Unmarshal([]byte(jsonStr), &list); err != nil {
		return nil, err
	}
	var out []ResolvedCheck
	for i := range list {
		jc := &list[i]
		if jc.Name == "" || jc.Kind == "" {
			continue
		}
		rc := ResolvedCheck{Name: jc.Name, Kind: strings.ToLower(jc.Kind)}
		if jc.Value != nil {
			rc.URL = resolve(env, jc.Value.URL)
			if m := strings.ToUpper(strings.TrimSpace(jc.Value.Method)); m != "" {
				rc.Method = m
			}
			rc.User = resolve(env, jc.Value.User)
			rc.Pass = resolve(env, jc.Value.Pass)
			rc.Host = resolve(env, jc.Value.Host)
			rc.Db = resolve(env, jc.Value.Db)
			rc.Path = resolve(env, jc.Value.Path)
			rc.Mode = jc.Value.Mode
			rc.StatusCode = jc.Value.StatusCode
			rc.NotContains = jc.Value.NotContains
			rc.TimeoutMs = jc.Value.TimeoutMs
			rc.IntervalSecs = jc.Value.IntervalSecs
			rc.ActionUrl = jc.Value.ActionUrl
			rc.ActionMethod = jc.Value.ActionMethod
			rc.ActionHeaders = jc.Value.ActionHeaders
			rc.ActionWaitForIncident = jc.Value.ActionWaitForIncident
			rc.ActionOnClose = jc.Value.ActionOnClose
			if jc.Value.Contains != "" {
				rc.Contains = jc.Value.Contains
			} else {
				applyResult(&rc, jc.Value.Result)
			}
		}
		if rc.Kind == "mysql" && rc.URL == "" && rc.Host != "" && rc.Db != "" {
			rc.URL = buildMysqlURL(rc.Host, rc.Db)
		}
		// Redis/postgres/mongodb/memcached: host field is used directly (host:port); no URL assembly needed.
		out = append(out, rc)
	}
	return out, nil
}

func resolve(env map[string]string, v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if m, ok := v.(map[string]interface{}); ok {
		if val, ok := m["value"]; ok && val != nil {
			if s, ok := val.(string); ok {
				return s
			}
		}
		if fromEnv, ok := m["fromEnv"]; ok && fromEnv != nil {
			key := ""
			if s, ok := fromEnv.(string); ok {
				key = s
			}
			if key != "" && env != nil {
				return env[key]
			}
		}
		// fromEnvInstanceSuffix mirrors BasicDataSource.setUsername() in the Java apps:
		// reads the base username from the env var, then appends the pod ordinal (from POD_NAME)
		// in the same position the Java app does — before any "-" suffix in the base name.
		// e.g. base="tomcat", POD_NAME="web-site-1"  → "tomcat1"
		//      base="tomcat-read",                         → "tomcat1-read"
		if fromEnvIS, ok := m["fromEnvInstanceSuffix"]; ok && fromEnvIS != nil {
			key := ""
			if s, ok := fromEnvIS.(string); ok {
				key = s
			}
			if key != "" && env != nil {
				base := env[key]
				podName := env["POD_NAME"]
				ordinal := ""
				if idx := strings.LastIndex(podName, "-"); idx != -1 {
					ordinal = podName[idx+1:]
				}
				suffix := ""
				if i := strings.Index(base, "-"); i != -1 {
					suffix = base[i:]
					base = base[:i]
				}
				return base + ordinal + suffix
			}
		}
	}
	return ""
}

func buildMysqlURL(hostPort, db string) string {
	return "mysql://" + hostPort + "/" + db
}

// Load loads from os.Environ() (key=value) into a map and calls LoadFromEnv.
func Load() ([]ResolvedCheck, error) {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		i := strings.Index(e, "=")
		if i <= 0 {
			continue
		}
		env[e[:i]] = e[i+1:]
	}
	return LoadFromEnv(env)
}

// ResolveManagerChecks converts manager DB checks (from GET /api/deployment-health-checks/yaml) to resolved checks using env. Same shape as HC_CHECKS_JSON.
func ResolveManagerChecks(checks []models.JsonCheck, env map[string]string) []ResolvedCheck {
	if len(checks) == 0 || env == nil {
		return nil
	}
	var out []ResolvedCheck
	for i := range checks {
		jc := &checks[i]
		if jc.Name == "" || jc.Kind == "" {
			continue
		}
		rc := ResolvedCheck{Name: jc.Name, Kind: strings.ToLower(jc.Kind), FromDB: true}
		if jc.Value != nil {
			rc.URL = resolve(env, jc.Value.URL)
			// HTTP method (GET/HEAD/POST) configured in the manager UI - previously dropped here,
			// so every manager-defined HTTP check ran as GET.
			if m := strings.ToUpper(strings.TrimSpace(jc.Value.Method)); m != "" {
				rc.Method = m
			}
			rc.User = resolve(env, jc.Value.User)
			rc.Pass = resolve(env, jc.Value.Pass)
			rc.Host = resolve(env, jc.Value.Host)
			rc.Db = resolve(env, jc.Value.Db)
			rc.Path = resolve(env, jc.Value.Path)
			rc.Mode = jc.Value.Mode
			rc.StatusCode = jc.Value.StatusCode
			rc.NotContains = jc.Value.NotContains
			rc.TimeoutMs = jc.Value.TimeoutMs
			rc.IntervalSecs = jc.Value.IntervalSecs
			rc.ActionUrl = jc.Value.ActionUrl
			rc.ActionMethod = jc.Value.ActionMethod
			rc.ActionHeaders = jc.Value.ActionHeaders
			rc.ActionWaitForIncident = jc.Value.ActionWaitForIncident
			rc.ActionOnClose = jc.Value.ActionOnClose
			if jc.Value.Contains != "" {
				rc.Contains = jc.Value.Contains
			} else {
				applyResult(&rc, jc.Value.Result)
			}
		}
		if rc.Kind == "mysql" && rc.URL == "" && rc.Host != "" && rc.Db != "" {
			rc.URL = buildMysqlURL(rc.Host, rc.Db)
		}
		// Redis/postgres/mongodb/memcached: host field is used directly (host:port); no URL assembly needed.
		out = append(out, rc)
	}
	return out
}

// applyResult maps the legacy "result" field to either StatusCode or Contains.
// Old check definitions used "result" to mean the expected HTTP status code (e.g. "200").
// If the value is purely numeric and in the 100–599 range, treat it as StatusCode.
// Otherwise treat it as a body Contains string (e.g. "UP").
// StatusCode set via the explicit "statusCode" field always wins.
func applyResult(rc *ResolvedCheck, result string) {
	if result == "" {
		return
	}
	// Check if it is a pure numeric HTTP status code (100–599)
	n := 0
	for _, c := range result {
		if c < '0' || c > '9' {
			n = -1
			break
		}
		n = n*10 + int(c-'0')
	}
	if n >= 100 && n <= 599 {
		// Looks like a status code — only apply if no explicit statusCode was set
		if rc.StatusCode == "" {
			rc.StatusCode = result
		}
	} else {
		rc.Contains = result
	}
}
