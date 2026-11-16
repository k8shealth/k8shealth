package health

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fx/health-agent-go/models"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestDefaultProbesAreAllValid(t *testing.T) {
	var raw []map[string]interface{}
	if err := json.Unmarshal(defaultSoftwareProbesJSON, &raw); err != nil {
		t.Fatal(err)
	}
	probes, err := parseSoftwareProbes(defaultSoftwareProbesJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != len(raw) {
		t.Fatalf("some default probes were rejected: %d valid of %d", len(probes), len(raw))
	}
}

func TestInvalidProbesAreDropped(t *testing.T) {
	js := `[
	 {"id":"ok","kind":"env","names":["JAVA_VERSION"],"extract":[{"product":"java","regex":"=(\\d+)"}]},
	 {"id":"bad id!","kind":"env","names":["A"],"extract":[{"product":"java","regex":"=(\\d+)"}]},
	 {"id":"inj","kind":"cmd","paths":["java"],"arg":"-v; rm -rf /","extract":[{"product":"java","regex":"(\\d+)"}]},
	 {"id":"badpath","kind":"file","paths":["/etc/x y"],"extract":[{"product":"java","regex":"(\\d+)"}]},
	 {"id":"nogroup","kind":"env","names":["A"],"extract":[{"product":"java","regex":"\\d+"}]},
	 {"id":"badre","kind":"env","names":["A"],"extract":[{"product":"java","regex":"(["}]},
	 {"id":"badprod","kind":"env","names":["A"],"extract":[{"product":"Java;","regex":"(\\d+)"}]},
	 {"id":"nokind","kind":"shell","extract":[{"product":"java","regex":"(\\d+)"}]},
	 {"id":"ok","kind":"env","names":["DUP"],"extract":[{"product":"java","regex":"=(\\d+)"}]},
	 {"id":"jarbad","kind":"jar","jarMode":"exec","jarEntry":"x","extract":[{"product":"a","regex":"(\\d+)"}]}
	]`
	probes, err := parseSoftwareProbes([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 1 || probes[0].ID != "ok" || probes[0].Names[0] != "JAVA_VERSION" {
		t.Fatalf("expected only the first valid probe, got %+v", probes)
	}
	if _, err := parseSoftwareProbes([]byte("not json")); err == nil {
		t.Fatal("expected error for garbage input")
	}
}

func TestProbeSpecFormatAndDisable(t *testing.T) {
	t.Setenv("HEALTH_AGENT_SOFTWARE_INTERVAL_SEC", "900")
	spec := SoftwareProbeSpec()
	if !strings.HasPrefix(spec, "interval=900\nmaxbytes=4096\ntimeout=3\nmaxtotal=262144\n") {
		t.Fatalf("unexpected header: %q", spec)
	}
	for _, want := range []string{"env|base-image-env|JAVA_VERSION,", "file|os-release|/etc/os-release;/usr/lib/os-release", "cmd|nginx-version|nginx|-v", "jar|jar-libs|list|BOOT-INF/lib/", "file|java-release|$ENV:JAVA_HOME/release;"} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec missing %q", want)
		}
	}
	if strings.Contains(spec, "\r") {
		t.Error("spec must be LF only")
	}
	t.Setenv("HEALTH_AGENT_DETECT_SOFTWARE", "false")
	if SoftwareProbeSpec() != "" {
		t.Error("spec must be empty when detection is disabled")
	}
	t.Setenv("HEALTH_AGENT_SOFTWARE_INTERVAL_SEC", "5") // below minimum -> default
	if softwareIntervalSec() != 6*3600 {
		t.Error("out-of-range interval must fall back to default")
	}
}

func TestParseSoftwareFacts(t *testing.T) {
	probes := SoftwareProbes()
	osr := "PRETTY_NAME=\"Debian GNU/Linux 11 (bullseye)\"\nVERSION_ID=\"11\"\nID=debian\n"
	res := []SoftwareFactResult{
		{ID: "base-image-env", B64: b64("JAVA_VERSION=17.0.9\nPHP_VERSION=8.1.2\nNODE_VERSION=18.19.0\n")},
		{ID: "os-release", B64: b64(osr)},
		{ID: "java-release", B64: b64("IMPLEMENTOR=\"x\"\nJAVA_VERSION=\"17.0.9\"\n")},
		{ID: "jar-manifest", B64: b64("Manifest-Version: 1.0\nSpring-Boot-Version: 2.7.18\nStart-Class: x\n")},
		{ID: "jar-libs", B64: b64("BOOT-INF/lib/spring-core-5.3.31.jar\nBOOT-INF/lib/tomcat-embed-core-9.0.83.jar\nBOOT-INF/lib/jetty-server-bad.jar\n")},
		{ID: "nginx-version", B64: b64("nginx version: nginx/1.18.0 (Ubuntu)\n")},
		{ID: "unknown-probe", B64: b64("x")},
		{ID: "wordpress-core", B64: "!!!not base64!!!"},
	}
	got := ParseSoftwareFacts(probes, res, "app")
	want := map[string]string{
		"java": "17.0.9", "php": "8.1.2", "nodejs": "18.19.0", "debian": "11",
		"spring-boot": "2.7.18", "spring-framework": "5.3.31", "tomcat": "9.0.83", "nginx": "1.18.0",
	}
	have := map[string]string{}
	for _, d := range got {
		have[d.Product] = d.Version
		if d.Container != "app" || d.Port != 0 {
			t.Errorf("container/port not set on %+v", d)
		}
	}
	for p, v := range want {
		if have[p] != v {
			t.Errorf("%s: want %q got %q (all: %+v)", p, v, have[p], got)
		}
	}
	if len(got) != len(want) { // java appears twice (env + release) but is de-duplicated
		t.Errorf("expected %d unique detections, got %d: %+v", len(want), len(got), got)
	}
}

func TestJavaVersionFormats(t *testing.T) {
	probes := SoftwareProbes()
	cases := []struct{ name, id, text, want string }{
		{"env modern jdk", "base-image-env", "JAVA_VERSION=jdk-11.0.31+11\n", "11.0.31"},
		{"env temurin 8", "base-image-env", "JAVA_VERSION=jdk8u482-b08\n", "8.0.482"},
		{"env short 8", "base-image-env", "JAVA_VERSION=8u191\n", "8.0.191"},
		{"env plain", "base-image-env", "JAVA_VERSION=17.0.1\n", "17.0.1"},
		{"env four segments", "base-image-env", "JAVA_VERSION=21.0.12.1\n", "21.0.12.1"},
		{"release legacy", "java-release", "JAVA_VERSION=\"1.8.0_321\"\n", "8.0.321"},
		{"release modern", "java-release", "JAVA_VERSION=\"17.0.18\"\nJAVA_RUNTIME_VERSION=\"17.0.18+8\"\n", "17.0.18"},
	}
	for _, c := range cases {
		got := ParseSoftwareFacts(probes, []SoftwareFactResult{{ID: c.id, B64: b64(c.text)}}, "app")
		if len(got) != 1 || got[0].Product != "java" || got[0].Version != c.want {
			t.Errorf("%s: want java %s, got %+v", c.name, c.want, got)
		}
	}
}

// gdpr-style image: a stale JAVA_VERSION env (7u171) while the JDK that actually runs is 1.8.0_321.
func TestReleaseFileBeatsStaleEnv(t *testing.T) {
	probes := SoftwareProbes()
	got := ParseSoftwareFacts(probes, []SoftwareFactResult{
		{ID: "base-image-env", B64: b64("JAVA_VERSION=7u171\nTOMCAT_VERSION=7.0.86\n")},
		{ID: "java-release", B64: b64("JAVA_VERSION=\"1.8.0_321\"\n")},
	}, "app")
	have := map[string]string{}
	for _, d := range got {
		have[d.Product] = d.Version
	}
	if have["java"] != "8.0.321" || len(got) != 2 {
		t.Fatalf("release file must win over env for java (tomcat kept): %+v", got)
	}
}

func TestJvmLibraries(t *testing.T) {
	probes := SoftwareProbes()
	libs := "BOOT-INF/lib/log4j-core-2.14.1.jar\nBOOT-INF/lib/kotlin-stdlib-1.9.25.jar\nBOOT-INF/lib/hibernate-core-6.4.4.Final.jar\nBOOT-INF/lib/log4j-api-2.14.1.jar\n"
	got := ParseSoftwareFacts(probes, []SoftwareFactResult{{ID: "jar-libs", B64: b64(libs)}}, "app")
	have := map[string]string{}
	for _, d := range got {
		have[d.Product] = d.Version
	}
	want := map[string]string{"log4j": "2.14.1", "kotlin": "1.9.25", "hibernate-orm": "6.4.4"}
	for p, v := range want {
		if have[p] != v {
			t.Errorf("%s: want %s got %q (%+v)", p, v, have[p], got)
		}
	}
}

func TestOsReleaseRequiresMatchingDistro(t *testing.T) {
	probes := SoftwareProbes()
	got := ParseSoftwareFacts(probes, []SoftwareFactResult{{ID: "os-release", B64: b64("ID=alpine\nVERSION_ID=3.14.2\n")}}, "c")
	if len(got) != 1 || got[0].Product != "alpine-linux" || got[0].Version != "3.14.2" {
		t.Fatalf("alpine: %+v", got)
	}
	if got := ParseSoftwareFacts(probes, []SoftwareFactResult{{ID: "os-release", B64: b64("ID=weirdos\nVERSION_ID=9\n")}}, "c"); len(got) != 0 {
		t.Fatalf("unknown distro must not be reported: %+v", got)
	}
}

func TestFactsRejectBadVersionsAndHugeInput(t *testing.T) {
	probes := SoftwareProbes()
	greedy, _ := parseSoftwareProbes([]byte(`[{"id":"g","kind":"env","names":["A"],"extract":[{"product":"php","regex":"=(\\S+)"}]}]`))
	if got := ParseSoftwareFacts(greedy, []SoftwareFactResult{{ID: "g", B64: b64("A=8.1.2;rm\n")}}, "c"); len(got) != 0 {
		t.Fatalf("version with junk must be rejected: %+v", got)
	}
	huge := strings.Repeat("A", 5*maxFactDecodedBytes) + "\nPHP_VERSION=8.1.2\n"
	if got := ParseSoftwareFacts(probes, []SoftwareFactResult{{ID: "base-image-env", B64: b64(huge)}}, "c"); len(got) != 0 {
		t.Fatalf("input beyond the cap must be truncated, not parsed: %+v", got)
	}
}

func TestIngestAndMerge(t *testing.T) {
	sc := NewSoftwareScanner(8082)
	SetDefaultSoftwareScanner(sc)
	defer SetDefaultSoftwareScanner(nil)
	sc.results = []models.DetectedSoftware{{Port: 80, Product: "apache-http-server", Version: "2.4.57", Confidence: "exact", Source: "http-header"}}

	body := `{"pod":"p","container":"app","files":[],"softwareFacts":[{"id":"base-image-env","b64":"` + b64("PHP_VERSION=8.1.2\n") + `"},{"id":"nginx-version","b64":"` + b64("nginx/1.25.3") + `"}]}`
	IngestSoftwareFacts([]byte(body), "app")
	IngestSoftwareFacts([]byte(`{"pod":"p","softwareFacts":"garbage"}`), "other") // malformed: ignored
	IngestSoftwareFacts([]byte(`{"pod":"p"}`), "none")                           // absent: ignored

	got := sc.Get()
	if len(got) != 3 || got[0].Product != "apache-http-server" {
		t.Fatalf("merge result wrong: %+v", got)
	}
	seen := map[string]string{}
	for _, d := range got {
		seen[d.Product] = d.Container
	}
	if seen["php"] != "app" || seen["nginx"] != "app" {
		t.Fatalf("container not propagated: %+v", got)
	}

	// same product+version already known from a port: not duplicated
	sc.SetContainerFacts("web", []models.DetectedSoftware{{Product: "apache-http-server", Version: "2.4.57", Container: "web"}})
	if n := len(sc.Get()); n != 3 {
		t.Fatalf("duplicate not suppressed, got %d", n)
	}

	// expiry
	sc.mu.Lock()
	cf := sc.facts["app"]
	cf.at = time.Now().Add(-100 * time.Hour)
	sc.facts["app"] = cf
	sc.mu.Unlock()
	if n := len(sc.Get()); n != 1 {
		t.Fatalf("expired facts must be dropped, got %d", n)
	}
}

func TestGenericEnvVersions(t *testing.T) {
	probes, _ := parseSoftwareProbes([]byte(`[{"id":"env-versions","kind":"envall"}]`))
	if len(probes) != 1 {
		t.Fatalf("envall probe must be valid without names/extract, got %d", len(probes))
	}
	env := "MYSQL_VERSION=8.0.36\nPG_VERSION=15.6\nPERL_VERSION=5.38.2\nMONGO_VERSION=6.0.14\nNODE_VERSION=20.11.1\n" +
		"APP_VERSION=1.4.2\nPYTHON_PIP_VERSION=23.0.1\nGOSU_VERSION=1.17\nJAVA_VERSION=8u482\nBAD_VERSION=latest\n" +
		"SECRET_TOKEN=abc\nHTTPD_VERSION=2.4.62\nYARN_VERSION=1.22.22\nOPENSSL_VERSION=1.1.0f-3\nJAVA_ALPINE_VERSION=8.191.12\nSTRIMZI_VERSION=0.42.0\n"
	got := ParseSoftwareFacts(probes, []SoftwareFactResult{{ID: "env-versions", B64: b64(env)}}, "db")
	have := map[string]string{}
	for _, d := range got {
		have[d.Product] = d.Version
		if d.Source != "container-env" || d.Confidence != "probable" || d.Container != "db" {
			t.Fatalf("unexpected metadata: %+v", d)
		}
	}
	want := map[string]string{"mysql": "8.0.36", "pg": "15.6", "perl": "5.38.2", "mongo": "6.0.14",
		"nodejs": "20.11.1", "java": "8.0.482", "apache-http-server": "2.4.62"}
	for k, v := range want {
		if have[k] != v {
			t.Fatalf("%s: want %q got %q (all: %v)", k, v, have[k], have)
		}
	}
	for _, k := range []string{"app", "python-pip", "gosu", "bad", "secret-token", "yarn", "openssl", "java-alpine", "strimzi"} {
		if _, ok := have[k]; ok {
			t.Fatalf("%s must not be reported (all: %v)", k, have)
		}
	}
}

func TestGenericEnvDedupesAgainstExplicitProbe(t *testing.T) {
	probes := SoftwareProbes() // defaults: base-image-env + env-versions both report NODE_VERSION
	text := "NODE_VERSION=20.11.1\nPHP_VERSION=8.2.10\n"
	got := ParseSoftwareFacts(probes, []SoftwareFactResult{
		{ID: "base-image-env", B64: b64(text)},
		{ID: "env-versions", B64: b64(text)},
	}, "app")
	n := map[string]int{}
	for _, d := range got {
		n[d.Product+"|"+d.Version]++
	}
	if n["nodejs|20.11.1"] != 1 || n["php|8.2.10"] != 1 {
		t.Fatalf("explicit and generic results must be merged, got %v", n)
	}
}
