package health

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/registry"
)

var springPlaceholderRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// resolveSpringPlaceholders replaces ${VAR_NAME} Spring-style placeholders with
// the matching environment variable value. Unresolved placeholders are left as-is.
func resolveSpringPlaceholders(s string, env map[string]string) string {
	return springPlaceholderRe.ReplaceAllStringFunc(s, func(m string) string {
		key := m[2 : len(m)-1]
		if v, ok := env[key]; ok {
			return v
		}
		return m
	})
}

// formatResolvedCheck formats a ResolvedCheck like Java HealthCheck.toString(): HealthCheck{index=0, name='...', kind='...', ...}
func formatResolvedCheck(index int, c ResolvedCheck) string {
	mode := c.Mode
	if mode == "" {
		mode = "null"
	}
	resolvedPath := c.Path
	if resolvedPath == "" {
		resolvedPath = "null"
	} else {
		resolvedPath = "'" + resolvedPath + "'"
	}
	resolvedPass := "***"
	if c.Pass == "" {
		resolvedPass = "null"
	}
	return fmt.Sprintf("HealthCheck{index=%d, name='%s', kind='%s', description='null', mode='%s', url=null, user=empty, pass=empty, host=empty, db=empty, path=empty, resolvedUrl='%s', resolvedUser='%s', resolvedPass='%s', resolvedHost='%s', resolvedDb='%s', resolvedPath=%s, resolvedMode='%s', expectBodyContains='null'}",
		index, c.Name, c.Kind, mode, c.URL, c.User, resolvedPass, c.Host, c.Db, resolvedPath, mode)
}

const defaultCheckIntervalSecs = 5
const checkInterval = defaultCheckIntervalSecs * time.Second

// RunHealthChecks loads HC_CHECKS_JSON and runs http/mysql checks periodically.
// Each check respects its own intervalSecs; checks with no interval use the global 5s default.
// Failing checks always retry at the default 5s interval regardless of intervalSecs, so a
// startup "connection refused" doesn't leave a check stuck until the full configured period elapses.
func RunHealthChecks(cfg *config.Config, reg *registry.Registry) {
	// lastRun tracks when each check (by name) last executed so per-check intervals work.
	lastRun := make(map[string]time.Time)
	// lastFailed tracks whether the previous run of a check resulted in a failure.
	lastFailed := make(map[string]bool)
	// consecutiveFails counts uninterrupted failing runs per check name.
	// Used when ActionOnDetection=false to fire the action only after 2 consecutive
	// failures (simulating the manager's incident-open threshold).
	consecutiveFails := make(map[string]int)
	for {
		runOnce(cfg, reg, lastRun, lastFailed, consecutiveFails)
		time.Sleep(checkInterval)
	}
}

func runOnce(cfg *config.Config, reg *registry.Registry, lastRun map[string]time.Time, lastFailed map[string]bool, consecutiveFails map[string]int) {
	checks, err := Load()
	if err != nil {
			config.LogErrorf("Health checks load: %v", err)
		return
	}
	// Merge manual health checks from manager DB (pulled in manager sync, run constantly like HC_CHECKS_JSON).
	// Manager sends annotation checks + DB checks together. DB overrides share the same name as the annotation
	// check they replace. Deduplicate by name keeping the LAST entry so DB overrides win over annotation checks
	// and duplicates (annotation check from Load() and again from manager) are collapsed to one.
	env := envMap()
	managerChecks := reg.GetManagerHealthChecks()
	if len(managerChecks) > 0 {
		resolved := ResolveManagerChecks(managerChecks, env)
		checks = append(checks, resolved...)
	}
	// Deduplicate the merged check list so annotation checks never run in parallel with a DB override
	// that targets the same endpoint.  Two passes:
	//
	// Pass 1 — by name (exact match).  Handles same-name overrides: the DB check is appended after
	//          annotation checks, so it naturally wins when names match.
	//
	// Pass 2 — by endpoint identity (type + URL / host / path, case-insensitive, trailing-slash
	//          normalised).  Handles name-change overrides: a user renames the DB check in the UI
	//          but the annotation still has the old name → without this pass both would run.
	//          DB checks are always appended AFTER annotation checks, so within the same identity
	//          key the last entry is always the DB version — it wins.
	//
	// When a check is displaced and its URL changes, the stale registry entry must be pruned.
	//
	// displacedDbAliases maps winner name → []displaced DB check names sharing the same endpoint.
	// When two DB checks share an endpoint (different names, same URL), the older one is NOT pruned;
	// instead, after the winner runs, its result is mirrored to the alias so neither shows as Pending.
	var displacedURLs []string
	var displacedNames []string
	var displacedDbAliases map[string][]string
	if len(checks) > 0 {
		// Pass 1: name-based dedup.
		nameToIdx := make(map[string]int, len(checks))
		deduped := make([]ResolvedCheck, 0, len(checks))
		for _, c := range checks {
			if idx, ok := nameToIdx[c.Name]; ok {
				old := deduped[idx]
				// Do not replace a runnable check with an incomplete DB check that has no
				// connection info and therefore cannot execute.  A DB record with no URL/host
				// is effectively a no-op and must not displace a working annotation check.
			kind := strings.ToLower(c.Kind)
			incomplete := (kind == "mysql" || kind == "http") && c.URL == "" ||
				(kind == "redis" || kind == "postgres" || kind == "mongodb" || kind == "kafka") && c.Host == ""
				if incomplete {
					// New entry is incomplete — keep the existing (better) check.
					continue
				}
				if old.URL != "" && old.URL != c.URL {
					displacedURLs = append(displacedURLs, old.URL)
				}
				deduped[idx] = c
			} else {
				nameToIdx[c.Name] = len(deduped)
				deduped = append(deduped, c)
			}
		}
		checks = deduped

		// Pass 2: endpoint-identity dedup.
		// Also tracks displaced names (same endpoint, different name) so the stale registry
		// entry can be pruned — making DB override rename behave like a YAML rename + redeploy.
		idToIdx := make(map[string]int, len(checks))
		deduped2 := make([]ResolvedCheck, 0, len(checks))
		for _, c := range checks {
			key := endpointKey(c)
			if key == "" {
				deduped2 = append(deduped2, c)
				continue
			}
			if idx, ok := idToIdx[key]; ok {
				old := deduped2[idx]
				if old.URL != "" && old.URL != c.URL {
					displacedURLs = append(displacedURLs, old.URL)
				} else if old.Name != c.Name {
				if old.FromDB && c.FromDB {
					// Both checks are from the manager DB (user created two DB checks for the same
					// endpoint under different names).  Don't prune the old name — instead alias it:
					// after the winner runs its result is mirrored to the displaced name so that
					// neither check shows as Pending in the dashboard.
					if displacedDbAliases == nil {
						displacedDbAliases = make(map[string][]string)
					}
					displacedDbAliases[c.Name] = append(displacedDbAliases[c.Name], old.Name)
				} else {
					// YAML check displaced by a DB override → prune the stale YAML entry from
					// registry (same behaviour as a YAML rename + redeploy).
					displacedNames = append(displacedNames, old.Name)
				}
				}
				deduped2[idx] = c // later = DB version wins
			} else {
				idToIdx[key] = len(deduped2)
				deduped2 = append(deduped2, c)
			}
		}
		checks = deduped2
	}

	pod := reg.EffectivePodName()
	container := reg.EffectiveContainerName()
	if container == "" {
		container = reg.EffectiveDeployment()
	}
	if container == "" {
		container = pod
	}

	// Prune registry entries for annotation checks displaced by a DB override:
	// — different URL (same name): prune by URL so old HTTP result disappears.
	// — different name (same endpoint): prune by name so the old named entry disappears,
	//   matching the behaviour of a YAML rename + redeploy.
	for _, url := range displacedURLs {
		reg.PruneHTTPCheckByURL(pod, container, url)
	}
	for _, name := range displacedNames {
		reg.PruneCheckByName(pod, container, name)
	}

	// Merge auto-detected Spring Boot datasources (detected by health-agent.sh from the application JAR)
	springDS := reg.GetSpringDatasources(pod, container)
	activeSpringMysqlLower := make(map[string]struct{})
	for i, ds := range springDS {
		if ds.URL == "" {
			continue
		}
		// Same virtual path as health-manager Configs tab / exclusions (skip MySQL when user excluded that JAR properties file)
		if reg.IsExcludedPath(springJarApplicationPropertiesVirtualPath(ds.Profile)) {
			continue
		}
		name := "spring-db"
		if ds.Profile != "" {
			name = "spring-db-" + ds.Profile
		}
		if i > 0 {
			name = fmt.Sprintf("%s-%d", name, i+1)
		}
		activeSpringMysqlLower[strings.ToLower(name)] = struct{}{}
		checks = mergeSpringMysqlCheck(checks, ResolvedCheck{
			Name: name,
			Kind: "mysql",
			URL:  resolveSpringPlaceholders(ds.URL, env),
			User: resolveSpringPlaceholders(ds.Username, env),
			Pass: resolveSpringPlaceholders(ds.Password, env),
		})
	}
	reg.PruneSpringMysqlChecksOutsideSet(pod, container, activeSpringMysqlLower)
	now := time.Now()
	for _, c := range checks {
		// Determine this check's effective interval (minimum 5s).
		// File checks default to 30s — os.Stat is instant and files rarely change.
		// All other checks default to 5s.
		defaultInterval := defaultCheckIntervalSecs
		if strings.ToLower(c.Kind) == "file" {
			defaultInterval = 30
		}
		intervalSecs := c.IntervalSecs
		if intervalSecs < defaultCheckIntervalSecs {
			intervalSecs = defaultInterval
		}
		interval := time.Duration(intervalSecs) * time.Second

		// Failing checks retry at the configurable fast-retry interval (default 5s) so a startup
		// "connection refused" doesn't hold up recovery for the full configured intervalSecs.
		// The fast-retry interval is pushed from manager settings; falls back to 5s if not yet received.
		failRetrySecs := config.AgentSettings.GetCheckFailRetrySecs()
		if failRetrySecs <= 0 {
			failRetrySecs = defaultCheckIntervalSecs
		}
		failRetryInterval := time.Duration(failRetrySecs) * time.Second
		effectiveInterval := interval
		if lastFailed[c.Name] {
			effectiveInterval = failRetryInterval
		}
		// Skip if this check ran recently enough — honours per-check interval overrides.
		if last, ok := lastRun[c.Name]; ok && now.Sub(last) < effectiveInterval {
			config.LogDebugf("Skipping health check %q — interval %ds not elapsed (last ran %s ago)",
				c.Name, intervalSecs, now.Sub(last).Round(time.Second))
			continue
		}
		lastRun[c.Name] = now

		config.LogDebugf("Running health check: %s", c.Name)
		var passed bool
		// A check that references an env var which is not set in this container cannot run: report that
		// (failed, naming the variable) instead of running it with an empty value.
		kindToRun := c.Kind
		if len(c.MissingEnv) > 0 {
			reportMissingEnv(pod, container, c, reg)
			kindToRun = ""
		} else {
			clearMissingEnvReport(pod, container, c.Name, reg)
		}
		switch kindToRun {
		case "http":
			passed = runHTTPCheck(pod, container, c, reg)
		case "mysql":
			passed = runMySQLCheck(pod, container, c, reg)
		case "redis":
			passed = runRedisCheck(pod, container, c, reg)
		case "postgres":
			passed = runPostgresCheck(pod, container, c, reg)
		case "mongodb":
			passed = runMongoCheck(pod, container, c, reg)
		case "kafka":
			passed = runKafkaCheck(pod, container, c, reg)
		case "file":
			passed = runFileCheck(pod, container, c, reg)
		}
		wasFailed := lastFailed[c.Name]
		lastFailed[c.Name] = !passed

		// Fire action URL on state transitions.
		// Default (ActionWaitForIncident=false): fire on the very first failure (pass→fail).
		// ActionWaitForIncident=true: fire only after 2 consecutive failures, mirroring
		// the manager's incident-open threshold ("on confirmed incident").
		if !passed {
			consecutiveFails[c.Name]++
			if !wasFailed && !c.ActionWaitForIncident {
				// First failure, on-detection mode — fire immediately.
				c.DeploymentName = reg.EffectiveDeployment()
				FireCheckAction(c, true, reg)
			} else if consecutiveFails[c.Name] == 2 && c.ActionWaitForIncident {
				// Second consecutive failure, on-incident mode — fire now.
				c.DeploymentName = reg.EffectiveDeployment()
				FireCheckAction(c, true, reg)
			}
		} else {
			if wasFailed {
				// Just recovered — fire close action (only if ActionOnClose is set).
				c.DeploymentName = reg.EffectiveDeployment()
				FireCheckAction(c, false, reg)
			}
			consecutiveFails[c.Name] = 0
		}

		// Mirror result to any DB alias names sharing the same endpoint (DB→DB displacement).
		// This prevents the displaced DB check from showing as Pending in the dashboard even
		// though the agent only runs the winning (newer) check.
		if len(displacedDbAliases) > 0 {
			if aliases, ok := displacedDbAliases[c.Name]; ok {
				reg.AliasHealthCheck(pod, container, c.Name, aliases)
			}
		}
	}
	for i, c := range checks {
			config.LogDebugf("%s", formatResolvedCheck(i, c))
	}
}

// mergeSpringMysqlCheck adds an auto-detected Spring datasource check to the list without letting it
// collide with a manager/annotation check of the same name.
//
// The manager UI can save a DB override for a detected check (e.g. to set a timeout) that carries only the
// name/host and no connection URL. Previously that incomplete entry ran first, returned immediately (empty
// URL -> no result recorded) and consumed the per-name interval slot, so the real detected check was always
// skipped and showed Pending forever.
//   - same-name check without a URL: fill in URL/user/password from the Spring properties, keeping the
//     override's timeout, interval and action settings;
//   - same-name check that already has a URL: the explicit definition wins, nothing is appended. When it
//     points at the same host:port as the detected datasource (the manager form splits the JDBC URL into
//     host + db but never knows the password), its empty user/password are inherited from the Spring config.
func mergeSpringMysqlCheck(checks []ResolvedCheck, sc ResolvedCheck) []ResolvedCheck {
	for i := range checks {
		if !strings.EqualFold(checks[i].Name, sc.Name) {
			continue
		}
		if strings.ToLower(checks[i].Kind) != "mysql" {
			return checks
		}
		if checks[i].URL == "" || mysqlHostPort(checks[i].URL) == mysqlHostPort(sc.URL) {
			if checks[i].URL == "" {
				checks[i].URL = sc.URL
			}
			if checks[i].User == "" {
				checks[i].User = sc.User
			}
			if checks[i].Pass == "" {
				checks[i].Pass = sc.Pass
			}
		}
		return checks
	}
	return append(checks, sc)
}

// springJarApplicationPropertiesVirtualPath returns the logical BOOT-INF path used for config exclusions (must match health-manager getPodConfigs).
func springJarApplicationPropertiesVirtualPath(profile string) string {
	p := strings.TrimSpace(profile)
	if p == "" {
		return "BOOT-INF/classes/application.properties"
	}
	return "BOOT-INF/classes/application-" + p + ".properties"
}

// endpointKey returns a stable identity key for a check based solely on its monitored endpoint
// (type + URL / host / path).  Used by Pass 2 deduplication so a DB override with a renamed
// check correctly displaces the annotation check for the same endpoint.
// Returns "" when the check has no meaningful identifier (skip dedup for that entry).
func endpointKey(c ResolvedCheck) string {
	switch strings.ToLower(c.Kind) {
	case "http":
		if c.URL != "" {
			return "http:" + strings.ToLower(strings.TrimRight(c.URL, "/"))
		}
	case "mysql":
		if c.Host != "" {
			return "mysql:" + strings.ToLower(c.Host)
		}
		if c.URL != "" {
			// Extract host from URL (mysql://host:port/db or jdbc:mysql://host:port/db)
			u := c.URL
			u = strings.TrimPrefix(u, "jdbc:mysql://")
			u = strings.TrimPrefix(u, "mysql://")
			if i := strings.Index(u, "/"); i >= 0 {
				u = u[:i]
			}
			if u != "" {
				return "mysql:" + strings.ToLower(u)
			}
		}
	case "redis":
		if c.Host != "" {
			return "redis:" + strings.ToLower(c.Host)
		}
	case "kafka":
		if c.Host != "" {
			return "kafka:" + strings.ToLower(c.Host)
		}
	case "postgres":
		if c.Host != "" {
			return "postgres:" + strings.ToLower(c.Host)
		}
		if c.URL != "" {
			u := strings.TrimPrefix(c.URL, "postgres://")
			u = strings.TrimPrefix(u, "postgresql://")
			if i := strings.Index(u, "/"); i >= 0 {
				u = u[:i]
			}
			if u != "" {
				return "postgres:" + strings.ToLower(u)
			}
		}
	case "mongodb":
		if c.Host != "" {
			h := strings.TrimPrefix(c.Host, "mongodb://")
			if i := strings.Index(h, "/"); i >= 0 {
				h = h[:i]
			}
			return "mongodb:" + strings.ToLower(h)
		}
	case "file":
		if c.Path != "" {
			return "file:" + c.Path
		}
	}
	return ""
}

func envMap() map[string]string {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		i := strings.Index(e, "=")
		if i <= 0 {
			continue
		}
		env[e[:i]] = e[i+1:]
	}
	return env
}

// mysqlHostPort returns the lower-cased host:port of a mysql:// or jdbc:mysql:// URL (default port 3306).
func mysqlHostPort(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "jdbc:"), "mysql://")
	if k := strings.IndexAny(u, "/?"); k >= 0 {
		u = u[:k]
	}
	if u != "" && !strings.Contains(u, ":") {
		u += ":3306"
	}
	return strings.ToLower(u)
}
