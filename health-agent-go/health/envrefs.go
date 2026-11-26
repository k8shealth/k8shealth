package health

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

// A check can reference an environment variable of the agent container instead of a literal value
// ({ "fromEnv": "DB_PASS" } in a saved check, "envRefs" on a UI test). If such a variable is not set the check
// cannot run, and the user must be told which one is missing - an empty value would otherwise make the check
// stay Pending (no host) or fail with a misleading error (access denied for user '').

// CapabilityEnvRefs is announced to the manager in AgentPayload.Capabilities: this agent resolves the "envRefs" of a
// UI test. The manager must not queue such a test for an agent that does not announce it (it would ignore the
// references and run the test with empty values).
const CapabilityEnvRefs = "envRefs"

// MissingEnvMessage is the text shown to the user for one or more unset variables.
func MissingEnvMessage(names []string) string {
	uniq := uniqueSorted(names)
	if len(uniq) == 1 {
		return fmt.Sprintf("Environment variable %s is not set in the agent container. Add it to the container's environment, or use a direct value.", uniq[0])
	}
	return fmt.Sprintf("Environment variables %s are not set in the agent container. Add them to the container's environment, or use direct values.", strings.Join(uniq, ", "))
}

func uniqueSorted(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	var out []string
	for _, n := range names {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// envRefReported remembers the checks that currently show a "variable not set" result, so that the result is
// removed again when the variable appears (the normal result is stored under a different identity: URL/path).
var (
	envRefReportedMu sync.Mutex
	envRefReported   = map[string]struct{}{}
)

func envRefKey(pod, container, name string) string { return pod + "\x00" + container + "\x00" + name }

// reportMissingEnv records a failed result for a check that references unset env vars.
func reportMissingEnv(pod, container string, c ResolvedCheck, reg *registry.Registry) {
	msg := MissingEnvMessage(c.MissingEnv)
	now := time.Now()
	kind := strings.ToLower(c.Kind)
	reg.AddHealthCheck(pod, container, kind, models.HealthCheck{
		Type:           kind,
		Name:           c.Name,
		Passed:         false,
		Time:           now,
		CheckStartedAt: now,
		Response:       msg,
		Message:        msg,
		Mode:           c.Mode,
	})
	envRefReportedMu.Lock()
	envRefReported[envRefKey(pod, container, c.Name)] = struct{}{}
	envRefReportedMu.Unlock()
}

// clearMissingEnvReport removes the "variable not set" result of a check that can run now.
func clearMissingEnvReport(pod, container, name string, reg *registry.Registry) {
	k := envRefKey(pod, container, name)
	envRefReportedMu.Lock()
	_, was := envRefReported[k]
	delete(envRefReported, k)
	envRefReportedMu.Unlock()
	if was {
		reg.PruneCheckByName(pod, container, name)
	}
}

// pendingEnvFields are the fields of a UI test that may reference an env var.
var pendingEnvFields = []string{"url", "host", "db", "user", "pass", "path"}

// ResolvePendingEnvRefs applies the "envRefs" of a UI test ({field: ENV_NAME}) to the test in place and returns
// the names of the variables that are not set. lookup is os.LookupEnv in production.
func ResolvePendingEnvRefs(test map[string]interface{}, lookup func(string) (string, bool)) []string {
	refs, _ := test["envRefs"].(map[string]interface{})
	if len(refs) == 0 {
		return nil
	}
	var missing []string
	for _, field := range pendingEnvFields {
		name, _ := refs[field].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		val, ok := lookup(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		if field == "url" && val != "" && !strings.Contains(val, "://") {
			val = "http://" + val // same default as a typed URL without a scheme
		}
		test[field] = val
	}
	return missing
}

// LookupAgentEnv is the production lookup for ResolvePendingEnvRefs.
func LookupAgentEnv(name string) (string, bool) { return os.LookupEnv(name) }
