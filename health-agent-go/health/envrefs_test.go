package health

import (
	"strings"
	"testing"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

func envRef(name string) map[string]interface{} { return map[string]interface{}{"fromEnv": name} }

func checksOf(reg *registry.Registry) map[string]models.HealthCheck {
	out := map[string]models.HealthCheck{}
	for _, h := range reg.GetHealthBaseChecks()["pod-1"]["app"] {
		out[h.Name] = h
	}
	return out
}

func runChecks(t *testing.T, reg *registry.Registry) {
	t.Helper()
	t.Setenv("HC_CHECKS_JSON", "")
	t.Setenv("HEALTH_AGENT_HC_CHECKS_JSON", "")
	runOnce(&config.Config{}, reg, map[string]time.Time{}, map[string]bool{}, map[string]int{})
}

// One check of every kind, each referencing an env var that is NOT set in the agent container.
func missingEnvChecks() []models.JsonCheck {
	v := func(f func(*models.JsonCheckValue)) *models.JsonCheckValue { x := &models.JsonCheckValue{}; f(x); return x }
	return []models.JsonCheck{
		{Name: "c-http", Kind: "http", Value: v(func(x *models.JsonCheckValue) { x.URL = envRef("HM_T_URL") })},
		{Name: "c-mysql", Kind: "mysql", Value: v(func(x *models.JsonCheckValue) { x.Host = envRef("HM_T_HOST"); x.Db = "d"; x.User = "u"; x.Pass = "p" })},
		{Name: "c-mysql-pass", Kind: "mysql", Value: v(func(x *models.JsonCheckValue) { x.Host = "127.0.0.1:1"; x.Db = "d"; x.User = "u"; x.Pass = envRef("HM_T_PASS") })},
		{Name: "c-redis", Kind: "redis", Value: v(func(x *models.JsonCheckValue) { x.Host = envRef("HM_T_RHOST") })},
		{Name: "c-postgres", Kind: "postgres", Value: v(func(x *models.JsonCheckValue) { x.Host = envRef("HM_T_PGHOST") })},
		{Name: "c-mongodb", Kind: "mongodb", Value: v(func(x *models.JsonCheckValue) { x.Host = envRef("HM_T_MGHOST") })},
		{Name: "c-kafka", Kind: "kafka", Value: v(func(x *models.JsonCheckValue) { x.Host = envRef("HM_T_KHOST") })},
		{Name: "c-file", Kind: "file", Value: v(func(x *models.JsonCheckValue) { x.Path = envRef("HM_T_PATH"); x.Mode = "read" })},
	}
}

var wantVar = map[string]string{
	"c-http": "HM_T_URL", "c-mysql": "HM_T_HOST", "c-mysql-pass": "HM_T_PASS", "c-redis": "HM_T_RHOST",
	"c-postgres": "HM_T_PGHOST", "c-mongodb": "HM_T_MGHOST", "c-kafka": "HM_T_KHOST", "c-file": "HM_T_PATH",
}

func TestRunner_UnsetEnvVarIsReportedAsFailureNamingTheVariable_EveryKind(t *testing.T) {
	reg := registry.New(&config.Config{PodName: "pod-1", Deployment: "dep", ContainerName: "app"})
	reg.SetManagerHealthChecks(missingEnvChecks())
	runChecks(t, reg)

	got := checksOf(reg)
	for name, variable := range wantVar {
		h, ok := got[name]
		if !ok {
			t.Errorf("%s: no result recorded (check would stay Pending and the user would never learn why)", name)
			continue
		}
		if h.Passed {
			t.Errorf("%s: passed although %s is not set", name, variable)
		}
		if !strings.Contains(h.Response, variable) || !strings.Contains(h.Response, "not set in the agent container") {
			t.Errorf("%s: response %q does not name %s", name, h.Response, variable)
		}
	}
}

func TestRunner_ResultOfUnsetVarDisappearsWhenVarIsSet(t *testing.T) {
	reg := registry.New(&config.Config{PodName: "pod-1", Deployment: "dep", ContainerName: "app"})
	reg.SetManagerHealthChecks(missingEnvChecks()[:1]) // c-http
	runChecks(t, reg)
	if h := checksOf(reg)["c-http"]; h.Passed || !strings.Contains(h.Response, "HM_T_URL") {
		t.Fatalf("expected the not-set failure first, got %+v", h)
	}

	t.Setenv("HM_T_URL", "http://127.0.0.1:1/health") // nothing listens: the check now really runs and fails to connect
	runChecks(t, reg)

	var n int
	for _, h := range reg.GetHealthBaseChecks()["pod-1"]["app"] {
		if h.Name != "c-http" {
			continue
		}
		n++
		if strings.Contains(h.Response, "not set") {
			t.Errorf("stale 'not set' result still shown after the variable was set: %q", h.Response)
		}
	}
	if n != 1 {
		t.Errorf("expected exactly one c-http result, got %d", n)
	}
}

func TestResolveManagerChecks_TracksOnlyUnsetVars(t *testing.T) {
	env := map[string]string{"SET": "x", "EMPTY": ""}
	checks := []models.JsonCheck{{Name: "a", Kind: "mysql", Value: &models.JsonCheckValue{
		Host: envRef("SET"), Db: envRef("GONE"), User: envRef("EMPTY"), Pass: envRef("GONE"),
	}}}
	rc := ResolveManagerChecks(checks, env)[0]
	if rc.Host != "x" {
		t.Errorf("host = %q", rc.Host)
	}
	// EMPTY is set (an empty password can be intended); GONE is reported once per use.
	if got := uniqueSorted(rc.MissingEnv); len(got) != 1 || got[0] != "GONE" {
		t.Errorf("missing = %v, want [GONE]", got)
	}
	if msg := MissingEnvMessage(rc.MissingEnv); !strings.Contains(msg, "GONE") || strings.Count(msg, "GONE") != 1 {
		t.Errorf("message should name GONE once: %q", msg)
	}
}

func TestMissingEnvMessage_PluralNamesAll(t *testing.T) {
	msg := MissingEnvMessage([]string{"B_VAR", "A_VAR"})
	if !strings.Contains(msg, "A_VAR, B_VAR") || !strings.HasPrefix(msg, "Environment variables ") {
		t.Errorf("unexpected message: %q", msg)
	}
}

func TestResolvePendingEnvRefs(t *testing.T) {
	lookup := func(name string) (string, bool) {
		switch name {
		case "HOST_VAR":
			return "db.local:3306", true
		case "EMPTY_VAR":
			return "", true
		}
		return "", false
	}
	test := map[string]interface{}{
		"host": "", "user": "u", "pass": "",
		"envRefs": map[string]interface{}{"host": "HOST_VAR", "pass": "EMPTY_VAR"},
	}
	if missing := ResolvePendingEnvRefs(test, lookup); len(missing) != 0 {
		t.Fatalf("unexpected missing: %v", missing)
	}
	if test["host"] != "db.local:3306" || test["pass"] != "" || test["user"] != "u" {
		t.Errorf("fields not applied: %v", test)
	}

	test = map[string]interface{}{"user": "u", "envRefs": map[string]interface{}{"user": "NOPE_VAR", "db": "NOPE2"}}
	missing := ResolvePendingEnvRefs(test, lookup)
	if len(missing) != 2 {
		t.Fatalf("want both unset vars reported, got %v", missing)
	}
	if test["user"] != "u" {
		t.Errorf("an unset variable must not overwrite the field: %v", test)
	}

	if m := ResolvePendingEnvRefs(map[string]interface{}{"host": "h"}, lookup); m != nil {
		t.Errorf("no envRefs => nothing to do, got %v", m)
	}
}
