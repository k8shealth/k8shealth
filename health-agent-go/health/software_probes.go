package health

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/models"
)

// Container software facts
//
// The agent sidecar cannot see the app container's files, but health-agent.sh already runs INSIDE every app
// container (Kyverno postStart hook, as the app's own user, no extra permissions). It is the "hands"; this
// file is the "brain":
//   - the probe RULES (what to read, how to turn it into product+version) live here as data
//     (software_probes_default.json, overridable via HEALTH_AGENT_SOFTWARE_PROBES_FILE / _JSON),
//   - the agent serves them to the script as a tiny pipe-separated spec (GET /api/software-probes),
//   - the script returns bounded raw text per probe (softwareFacts in its normal callback),
//   - the agent applies the regexes and merges the result into AgentPayload.detectedSoftware.
// Probe kinds are deliberately limited and validated (no shell, no arbitrary arguments):
//   env  - read named environment variables
//   file - read the first readable file from a candidate list
//   cmd  - run "<binary> <single dash-flag>" with a time limit
//   jar  - read an entry of / list the entries of the app's main jar

//go:embed software_probes_default.json
var defaultSoftwareProbesJSON []byte

const (
	maxSoftwareProbes   = 64
	maxExtractsPerProbe = 24
	maxFactDecodedBytes = 64 * 1024
	maxFactsPerPayload  = 128
	maxVersionLen       = 40
)

// SoftwareExtract turns the raw text of a probe into one product+version (first capture group = version).
type SoftwareExtract struct {
	Product    string `json:"product"`
	Regex      string `json:"regex"`
	Requires   string `json:"requires,omitempty"`   // optional: this regex must also match the text
	Confidence string `json:"confidence,omitempty"` // exact | probable | unknown (default by kind)

	re  *regexp.Regexp
	req *regexp.Regexp
}

// SoftwareProbe is one rule.
type SoftwareProbe struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Names    []string          `json:"names,omitempty"`    // env
	Paths    []string          `json:"paths,omitempty"`    // file: files, cmd: binaries ("$ENV:NAME/rest" expands an env var)
	Arg      string            `json:"arg,omitempty"`      // cmd: one dash flag, e.g. -v / --version
	JarMode  string            `json:"jarMode,omitempty"`  // jar: read | list
	JarEntry string            `json:"jarEntry,omitempty"` // jar: entry path / prefix
	Extract  []SoftwareExtract `json:"extract"`
}

var (
	reProbeID   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,48}$`)
	reEnvName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	rePathItem  = regexp.MustCompile(`^[A-Za-z0-9_./$:+@-]{1,200}$`)
	reCmdArg    = regexp.MustCompile(`^--?[A-Za-z]{1,16}$`)
	reJarEntry  = regexp.MustCompile(`^[A-Za-z0-9_./*-]{1,120}$`)
	reProductID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	reVersionOK = regexp.MustCompile(`^[0-9][0-9A-Za-z._+-]*$`)
)

var (
	probesOnce   sync.Once
	probesLoaded []SoftwareProbe
)

// SoftwareProbes returns the validated rules (loaded once).
func SoftwareProbes() []SoftwareProbe {
	probesOnce.Do(func() { probesLoaded = loadSoftwareProbes() })
	return probesLoaded
}

func loadSoftwareProbes() []SoftwareProbe {
	raw, src := defaultSoftwareProbesJSON, "built-in defaults"
	if v := strings.TrimSpace(os.Getenv("HEALTH_AGENT_SOFTWARE_PROBES_JSON")); v != "" {
		raw, src = []byte(v), "HEALTH_AGENT_SOFTWARE_PROBES_JSON"
	} else if p := strings.TrimSpace(os.Getenv("HEALTH_AGENT_SOFTWARE_PROBES_FILE")); p != "" {
		if b, err := os.ReadFile(p); err != nil {
			config.LogWarnf("[software-probes] cannot read %s (%v) - using built-in defaults", p, err)
		} else {
			raw, src = b, p
		}
	}
	probes, err := parseSoftwareProbes(raw)
	if err != nil || (len(probes) == 0 && src != "built-in defaults") {
		config.LogWarnf("[software-probes] %s unusable (%v) - using built-in defaults", src, err)
		probes, _ = parseSoftwareProbes(defaultSoftwareProbesJSON)
		src = "built-in defaults"
	}
	config.LogDebugf("[software-probes] %d probe(s) from %s", len(probes), src)
	return probes
}

// parseSoftwareProbes validates the rules. Invalid probes are dropped (never fatal), so a typo in an
// override file cannot break the agent or inject anything unexpected into the script.
func parseSoftwareProbes(raw []byte) ([]SoftwareProbe, error) {
	var in []SoftwareProbe
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	var out []SoftwareProbe
	seen := map[string]bool{}
	for _, p := range in {
		if len(out) >= maxSoftwareProbes {
			config.LogWarnf("[software-probes] more than %d probes - extra ignored", maxSoftwareProbes)
			break
		}
		clean, why := sanitizeProbe(p)
		if why != "" {
			config.LogWarnf("[software-probes] probe %q ignored: %s", p.ID, why)
			continue
		}
		if seen[clean.ID] {
			config.LogWarnf("[software-probes] duplicate probe id %q ignored", clean.ID)
			continue
		}
		seen[clean.ID] = true
		out = append(out, clean)
	}
	return out, nil
}

func sanitizeProbe(p SoftwareProbe) (SoftwareProbe, string) {
	if !reProbeID.MatchString(p.ID) {
		return p, "invalid id"
	}
	p.Kind = strings.ToLower(strings.TrimSpace(p.Kind))
	switch p.Kind {
	case "envall":
		// generic: every <NAME>_VERSION variable; products are derived from the names, no extract rules needed
		return p, ""
	case "env":
		if len(p.Names) == 0 || len(p.Names) > 64 {
			return p, "env needs 1..64 names"
		}
		for _, n := range p.Names {
			if !reEnvName.MatchString(n) {
				return p, "invalid env name " + strconv.Quote(n)
			}
		}
	case "file", "cmd":
		if len(p.Paths) == 0 || len(p.Paths) > 16 {
			return p, p.Kind + " needs 1..16 paths"
		}
		for _, x := range p.Paths {
			if !rePathItem.MatchString(x) {
				return p, "invalid path " + strconv.Quote(x)
			}
		}
		if p.Kind == "cmd" && !reCmdArg.MatchString(p.Arg) {
			return p, "cmd arg must be a single dash flag"
		}
	case "jar":
		p.JarMode = strings.ToLower(strings.TrimSpace(p.JarMode))
		if p.JarMode != "read" && p.JarMode != "list" {
			return p, "jarMode must be read or list"
		}
		if !reJarEntry.MatchString(p.JarEntry) {
			return p, "invalid jarEntry"
		}
	default:
		return p, "unknown kind " + strconv.Quote(p.Kind)
	}
	if len(p.Extract) == 0 || len(p.Extract) > maxExtractsPerProbe {
		return p, "needs 1.." + strconv.Itoa(maxExtractsPerProbe) + " extract rules"
	}
	exts := make([]SoftwareExtract, 0, len(p.Extract))
	for _, e := range p.Extract {
		if !reProductID.MatchString(e.Product) {
			return p, "invalid product " + strconv.Quote(e.Product)
		}
		re, err := regexp.Compile(e.Regex)
		if err != nil || re.NumSubexp() < 1 {
			return p, "bad regex for " + e.Product + " (must compile and have a capture group)"
		}
		e.re = re
		if e.Requires != "" {
			req, err := regexp.Compile(e.Requires)
			if err != nil {
				return p, "bad requires regex for " + e.Product
			}
			e.req = req
		}
		switch strings.ToLower(e.Confidence) {
		case "exact", "probable", "unknown":
			e.Confidence = strings.ToLower(e.Confidence)
		default:
			if p.Kind == "env" {
				e.Confidence = "probable"
			} else {
				e.Confidence = "exact"
			}
		}
		exts = append(exts, e)
	}
	p.Extract = exts
	return p, ""
}

// ---------- settings (all from the environment; defaults here only as fallbacks) ----------

func envInt(name string, def, min, max int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return def
	}
	return n
}

func softwareDetectionEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("HEALTH_AGENT_DETECT_SOFTWARE")), "false")
}

func softwareIntervalSec() int { return envInt("HEALTH_AGENT_SOFTWARE_INTERVAL_SEC", 6*3600, 300, 7*86400) }
func softwareMaxBytes() int    { return envInt("HEALTH_AGENT_SOFTWARE_MAX_BYTES", 4096, 512, 65536) }
func softwareCmdTimeoutSec() int {
	return envInt("HEALTH_AGENT_SOFTWARE_CMD_TIMEOUT_SEC", 3, 1, 30)
}
func softwareMaxTotalBytes() int {
	return envInt("HEALTH_AGENT_SOFTWARE_MAX_TOTAL_BYTES", 256*1024, 8192, 4*1024*1024)
}

// SoftwareProbeSpec renders the rules for health-agent.sh: header lines (key=value) then one
// pipe-separated line per probe. Empty when software detection is disabled or there are no rules.
func SoftwareProbeSpec() string {
	if !softwareDetectionEnabled() {
		return ""
	}
	probes := SoftwareProbes()
	if len(probes) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "interval=%d\nmaxbytes=%d\ntimeout=%d\nmaxtotal=%d\n",
		softwareIntervalSec(), softwareMaxBytes(), softwareCmdTimeoutSec(), softwareMaxTotalBytes())
	for _, p := range probes {
		switch p.Kind {
		case "envall":
			fmt.Fprintf(&b, "envall|%s|\n", p.ID)
		case "env":
			fmt.Fprintf(&b, "env|%s|%s\n", p.ID, strings.Join(p.Names, ","))
		case "file":
			fmt.Fprintf(&b, "file|%s|%s\n", p.ID, strings.Join(p.Paths, ";"))
		case "cmd":
			fmt.Fprintf(&b, "cmd|%s|%s|%s\n", p.ID, strings.Join(p.Paths, ";"), p.Arg)
		case "jar":
			fmt.Fprintf(&b, "jar|%s|%s|%s\n", p.ID, p.JarMode, p.JarEntry)
		}
	}
	return b.String()
}

// ---------- applying the rules to what the script returned ----------

// SoftwareFactResult is one probe result from the script (b64 = base64 of the raw, size-capped text).
type SoftwareFactResult struct {
	ID  string `json:"id"`
	B64 string `json:"b64"`
}

// ParseSoftwareFacts converts raw probe results into detected software. Unknown ids, bad base64 and
// non-matching text are skipped; the result is de-duplicated by product+version.
func ParseSoftwareFacts(probes []SoftwareProbe, results []SoftwareFactResult, container string) []models.DetectedSoftware {
	byID := make(map[string]*SoftwareProbe, len(probes))
	for i := range probes {
		byID[probes[i].ID] = &probes[i]
	}
	var out []models.DetectedSoftware
	seen := map[string]bool{}
	for i, r := range results {
		if i >= maxFactsPerPayload {
			break
		}
		p := byID[r.ID]
		if p == nil {
			continue
		}
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.B64))
		if err != nil || len(dec) == 0 {
			continue
		}
		if len(dec) > maxFactDecodedBytes {
			dec = dec[:maxFactDecodedBytes]
		}
		text := string(bytes.ToValidUTF8(dec, []byte("?")))
		if p.Kind == "envall" {
			for _, d := range genericEnvSoftware(text, container) {
				key := d.Product + "|" + d.Version
				if !seen[key] {
					seen[key] = true
					out = append(out, d)
				}
			}
			continue
		}
		for _, e := range p.Extract {
			if e.req != nil && !e.req.MatchString(text) {
				continue
			}
			m := e.re.FindStringSubmatch(text)
			if len(m) < 2 {
				continue
			}
			ver := normalizeVersion(e.Product, strings.TrimSpace(m[1]))
			if ver == "" || len(ver) > maxVersionLen || !reVersionOK.MatchString(ver) {
				continue
			}
			key := e.Product + "|" + ver
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, models.DetectedSoftware{
				Product:    e.Product,
				Version:    ver,
				Confidence: e.Confidence,
				Source:     "container-" + p.Kind,
				Container:  container,
			})
		}
	}
	return preferObservedFiles(out)
}

var reEnvVersionLine = regexp.MustCompile(`^([A-Z][A-Z0-9_]{1,40})_VERSION=(.+)$`)

// envProductAlias maps the prefix of a <NAME>_VERSION variable to the product name used everywhere else
// (the same names the explicit probes emit). Anything not listed becomes the lower-cased prefix ("PERL" -> "perl").
var envProductAlias = map[string]string{
	"NODE":   "nodejs",
	"GOLANG": "go",
	"HTTPD":  "apache-http-server",
}

// envNotSoftware lists prefixes that are an application's own/build version or packaging tooling, not software
// a release catalog could know about. Keeps the inventory free of "app 1.4.2" style noise.
var envNotSoftware = map[string]bool{
	"APP": true, "APPLICATION": true, "BUILD": true, "SERVICE": true, "IMAGE": true, "API": true, "RELEASE": true,
	"CHART": true, "GIT": true, "TAG": true, "SCHEMA": true, "PROJECT": true, "MODULE": true, "CONFIG": true,
	"DOCKER": true, "BASE": true, "COMMIT": true, "SOURCE": true, "SERVER": true, "CLIENT": true, "SDK": true,
	"GOSU": true, "TINI": true, "DUMB_INIT": true, "PYTHON_PIP": true, "PYTHON_SETUPTOOLS": true, "PYTHON_WHEEL": true,
	"PYTHON_GET_PIP": true, "NPM": true, "YARN_PKG": true,
	// bundled build tooling (the runtime's own row already covers the image) and operators' internal component versions
	"YARN": true, "NJS": true, "JAVA_ALPINE": true, "STRIMZI": true, "KAFKA_EXPORTER": true, "KAFKA_EXPORTER_KAFKA": true,
	// distro libraries: versions are backport-patched ("1.1.0f-3+deb9u2"), so comparing them with upstream releases misleads
	"OPENSSL": true, "LIBSSL": true, "ZLIB": true, "GLIBC": true, "CURL": true, "GNUPG": true,
}

const maxGenericEnvFacts = 32

// genericEnvSoftware turns "<NAME>_VERSION=<version>" lines into product+version pairs. Official images declare
// their software exactly like this (PERL_VERSION, MYSQL_VERSION, PG_VERSION, REDIS_VERSION, ...), so no per-product
// rule is needed. Whether a product is actually assessed is decided by the manager (release catalog lookup).
func genericEnvSoftware(text, container string) []models.DetectedSoftware {
	var out []models.DetectedSoftware
	for _, line := range strings.Split(text, "\n") {
		if len(out) >= maxGenericEnvFacts {
			break
		}
		m := reEnvVersionLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || envNotSoftware[m[1]] {
			continue
		}
		product, ok := envProductAlias[m[1]]
		if !ok {
			product = strings.ToLower(strings.ReplaceAll(m[1], "_", "-"))
		}
		if !reProductID.MatchString(product) {
			continue
		}
		ver := normalizeVersion(product, strings.TrimSpace(m[2]))
		if ver == "" || len(ver) > maxVersionLen || !reVersionOK.MatchString(ver) {
			continue
		}
		out = append(out, models.DetectedSoftware{
			Product:    product,
			Version:    ver,
			Confidence: "probable",
			Source:     "container-env",
			Container:  container,
		})
	}
	return out
}

var (
	reJavaLegacyUpdate = regexp.MustCompile(`^1\.(\d+)\.(\d+)_(\d+)$`) // 1.8.0_321 -> 8.0.321
	reJavaLegacy       = regexp.MustCompile(`^1\.(\d+)\.(\d+)$`)       // 1.8.0     -> 8.0
	reJavaShortUpdate  = regexp.MustCompile(`^(\d+)u(\d+)$`)           // 8u482     -> 8.0.482
)

// normalizeVersion brings vendor-specific version spellings to the dotted form the manager's release data uses.
// Only Java needs it: Java 8 reports "1.8.0_321" or "8u321", modern JDKs report "17.0.18" (unchanged).
func normalizeVersion(product, v string) string {
	if product != "java" {
		return v
	}
	if m := reJavaLegacyUpdate.FindStringSubmatch(v); m != nil {
		return m[1] + "." + m[2] + "." + m[3]
	}
	if m := reJavaLegacy.FindStringSubmatch(v); m != nil {
		return m[1] + "." + m[2]
	}
	if m := reJavaShortUpdate.FindStringSubmatch(v); m != nil {
		return m[1] + ".0." + m[2]
	}
	return v
}

// preferObservedFiles drops a product's env-derived version when a file-derived one exists for the same product:
// an image's JAVA_VERSION env can be stale (older base image, JDK swapped later) while the JDK's own release file
// describes what actually runs.
func preferObservedFiles(in []models.DetectedSoftware) []models.DetectedSoftware {
	fromFile := map[string]bool{}
	for _, d := range in {
		if d.Source == "container-file" {
			fromFile[d.Product] = true
		}
	}
	if len(fromFile) == 0 {
		return in
	}
	out := in[:0:0]
	for _, d := range in {
		if d.Source == "container-env" && fromFile[d.Product] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// ---------- container facts store + sink for the script callback ----------

type containerFacts struct {
	list []models.DetectedSoftware
	at   time.Time
}

// SetContainerFacts replaces the facts reported by one container's script.
func (s *SoftwareScanner) SetContainerFacts(container string, list []models.DetectedSoftware) {
	if container == "" {
		container = "container"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.facts == nil {
		s.facts = map[string]containerFacts{}
	}
	s.facts[container] = containerFacts{list: list, at: time.Now()}
}

var defaultSoftwareScanner atomic.Pointer[SoftwareScanner]

// SetDefaultSoftwareScanner registers the scanner that receives container facts from the script callback.
func SetDefaultSoftwareScanner(s *SoftwareScanner) { defaultSoftwareScanner.Store(s) }

// IngestSoftwareFacts reads the optional "softwareFacts" field of a raw script-callback body.
// It never panics and never returns an error: a malformed or absent field is simply ignored.
func IngestSoftwareFacts(body []byte, container string) {
	defer func() { _ = recover() }()
	if !softwareDetectionEnabled() || !bytes.Contains(body, []byte(`"softwareFacts"`)) {
		return
	}
	sc := defaultSoftwareScanner.Load()
	if sc == nil {
		return
	}
	var doc struct {
		SoftwareFacts *[]SoftwareFactResult `json:"softwareFacts"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.SoftwareFacts == nil {
		return
	}
	sc.SetContainerFacts(container, ParseSoftwareFacts(SoftwareProbes(), *doc.SoftwareFacts, container))
}

// mergedContainerFacts returns the non-expired container facts not already reported from a port
// (same product+version), sorted for stable output. Caller holds s.mu (read).
func (s *SoftwareScanner) mergedContainerFacts(have map[string]bool) []models.DetectedSoftware {
	if len(s.facts) == 0 {
		return nil
	}
	ttl := time.Duration(softwareIntervalSec()*3) * time.Second
	names := make([]string, 0, len(s.facts))
	for n := range s.facts {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []models.DetectedSoftware
	for _, n := range names {
		cf := s.facts[n]
		if time.Since(cf.at) > ttl {
			continue
		}
		for _, f := range cf.list {
			key := f.Product + "|" + f.Version
			if have[key] {
				continue
			}
			have[key] = true
			out = append(out, f)
		}
	}
	return out
}
