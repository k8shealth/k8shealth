package health

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/models"
)

// Software detection
//
// Finds out WHICH infrastructure software (Apache httpd, nginx, Tomcat, Jetty, HAProxy, MySQL, ...)
// and WHICH base version is listening inside this pod, so the manager can warn when it is outdated
// or end-of-life - independent of the container image tag (custom images hide the real version).
//
// How: read LISTEN sockets from /proc/net/tcp{,6} (pod network namespace is shared with the app),
// then fingerprint each port with a few read-only requests to the pod's own address:
//   - server-first protocols (MySQL/MariaDB greeting, SSH banner): just read what the server says
//   - HTTP: GET / and GET <random 404 path>, parse Server / X-Powered-By headers and error pages
//   - Redis: INFO server
// Nothing leaves the pod except the result list in the normal callback payload.
// No TLS probing (no certificate bypass), no authentication attempts, no plugin detection.
//
// Disable with HEALTH_AGENT_DETECT_SOFTWARE=false.

const (
	probeMaxBody   = 16 * 1024
	tcpStateListen = "0A"
)

// Tunables (defaults here are only fallbacks; every one can be set from the environment).
var (
	softwareScanInitialDelay = time.Duration(envInt("HEALTH_AGENT_DETECT_INITIAL_DELAY_SEC", 45, 0, 3600)) * time.Second
	probeDialTimeout         = time.Duration(envInt("HEALTH_AGENT_DETECT_DIAL_TIMEOUT_MS", 2000, 100, 30000)) * time.Millisecond
	probeBannerWait          = time.Duration(envInt("HEALTH_AGENT_DETECT_BANNER_WAIT_MS", 700, 100, 10000)) * time.Millisecond
	probeIOTimeout           = time.Duration(envInt("HEALTH_AGENT_DETECT_IO_TIMEOUT_MS", 3000, 200, 30000)) * time.Millisecond
	probeMaxPorts            = envInt("HEALTH_AGENT_DETECT_MAX_PORTS", 24, 1, 256)
)

// Ports of protocols we do not speak: probing them with HTTP would only produce error noise in
// the target's logs (Kafka "invalid receive", Postgres "invalid startup packet", ...).
// Override with HEALTH_AGENT_DETECT_SKIP_PORTS=comma,separated,ports (replaces this list).
var detectSkipPorts = map[int]bool{
	9092: true, 9093: true, // kafka
	5432: true, // postgres
	27017: true, 27018: true, // mongodb
	2181: true, 2888: true, 3888: true, // zookeeper
	5672: true, 15672: true, // rabbitmq (amqp)
	4222: true, // nats
	5701: true, 5702: true, 5703: true, // hazelcast
	11211: true, // memcached
	9042: true, // cassandra
	1433: true, // mssql
	1521: true, // oracle
	33060: true, 33061: true, // mysqlx / group replication
}

// SoftwareScanner periodically fingerprints the software listening in this pod.
type SoftwareScanner struct {
	mu        sync.RWMutex
	results   []models.DetectedSoftware
	facts     map[string]containerFacts // reported by health-agent.sh inside each app container
	skipPorts map[int]bool
}

// NewSoftwareScanner creates a scanner. agentPort (the agent's own HTTP port) is never probed.
func NewSoftwareScanner(agentPort int) *SoftwareScanner {
	skip := make(map[int]bool, len(detectSkipPorts)+1)
	if ov, set := os.LookupEnv("HEALTH_AGENT_DETECT_SKIP_PORTS"); set {
		for _, f := range strings.Split(ov, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n > 0 && n < 65536 {
				skip[n] = true
			}
		}
	} else {
		for p := range detectSkipPorts {
			skip[p] = true
		}
	}
	if agentPort > 0 {
		skip[agentPort] = true
	}
	return &SoftwareScanner{skipPorts: skip}
}

// AttachSoftwareScanner lets the sync layer reach the software scanner through the already-injected
// ConnectionScanner (keeps RunManagerSyncWithScanner's signature unchanged).
func (s *ConnectionScanner) AttachSoftwareScanner(sw *SoftwareScanner) { s.software = sw }

// GetSoftware returns the latest software fingerprint (nil when detection is off or not run yet).
func (s *ConnectionScanner) GetSoftware() []models.DetectedSoftware {
	if s.software == nil {
		return nil
	}
	return s.software.Get()
}

// Run is the scan loop. Call in a goroutine.
func (s *SoftwareScanner) Run() {
	if !softwareDetectionEnabled() {
		config.LogWarnf("software detection disabled (HEALTH_AGENT_DETECT_SOFTWARE=false)")
		return
	}
	time.Sleep(softwareScanInitialDelay) // let the app finish starting and open its ports
	for {
		s.scan()
		time.Sleep(time.Duration(softwareIntervalSec()) * time.Second)
	}
}

// Get returns a copy of the latest scan result (nil until the first scan completed).
func (s *SoftwareScanner) Get() []models.DetectedSoftware {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]models.DetectedSoftware, len(s.results))
	copy(out, s.results)
	have := make(map[string]bool, len(out))
	for _, r := range out {
		have[r.Product+"|"+r.Version] = true
	}
	// facts from inside the containers; a product+version already seen on a port is not repeated
	out = append(out, s.mergedContainerFacts(have)...)
	if len(out) == 0 {
		return nil
	}
	return out
}

type listenAddr struct {
	ip   string
	port int
}

func (s *SoftwareScanner) scan() {
	addrs := listenAddrs()
	var found []models.DetectedSoftware
	probed := 0
	for _, a := range addrs {
		if s.skipPorts[a.port] || a.port >= 32768 { // ephemeral range is never a service
			continue
		}
		if probed >= probeMaxPorts {
			break
		}
		probed++
		found = append(found, probePort(a)...)
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].Port != found[j].Port {
			return found[i].Port < found[j].Port
		}
		return found[i].Product < found[j].Product
	})
	s.mu.Lock()
	s.results = found
	s.mu.Unlock()
	config.LogDebugf("[software-detect] scanned %d port(s), %d detection(s)", probed, len(found))
}

// listenAddrs returns the distinct LISTEN ports (with their bind address) of this network namespace.
func listenAddrs() []listenAddr {
	seen := map[int]bool{}
	var out []listenAddr
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 4 || fields[3] != tcpStateListen {
				continue
			}
			ip, port, err := decodeHexAddr(fields[1], strings.HasSuffix(path, "tcp6"))
			if err != nil || port <= 0 || seen[port] {
				continue
			}
			seen[port] = true
			out = append(out, listenAddr{ip: ip, port: port})
		}
		f.Close()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].port < out[j].port })
	return out
}

func dialTarget(a listenAddr) string {
	host := a.ip
	if p := net.ParseIP(host); p == nil || p.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(a.port))
}

func probePort(a listenAddr) []models.DetectedSoftware {
	target := dialTarget(a)
	conn, err := net.DialTimeout("tcp", target, probeDialTimeout)
	if err != nil {
		return nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(probeBannerWait))
	buf := make([]byte, 512)
	n, rerr := conn.Read(buf)
	conn.Close()

	var res []models.DetectedSoftware
	if n > 0 {
		// server speaks first: MySQL / MariaDB / SSH ...
		res = parseServerBanner(buf[:n])
	} else if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
		// silent server: client-first protocol
		res = probeHTTP(target)
		if len(res) == 0 {
			res = probeRedis(target)
		}
	}
	for i := range res {
		res[i].Port = a.port
	}
	return res
}

// ---------- server-first banners ----------

var (
	reNumericVersion = regexp.MustCompile(`\d+(?:\.\d+){0,3}`)
	reMariaDBVersion = regexp.MustCompile(`(\d+\.\d+\.\d+)-MariaDB`)
	reSSHVersion     = regexp.MustCompile(`^SSH-2\.0-OpenSSH_([0-9]+\.[0-9]+)`)
)

func numericVersion(s string) string { return reNumericVersion.FindString(s) }

// parseServerBanner handles protocols where the server sends data right after connect.
func parseServerBanner(b []byte) []models.DetectedSoftware {
	// MySQL / MariaDB initial handshake: 3-byte length, 1-byte seq, protocol 0x0a, NUL-terminated version.
	if len(b) > 6 && b[4] == 0x0a {
		end := 5
		for end < len(b) && b[end] != 0 {
			end++
		}
		ver := string(b[5:end])
		if m := reMariaDBVersion.FindStringSubmatch(ver); m != nil {
			return []models.DetectedSoftware{{Product: "mariadb", Version: m[1], Confidence: "exact", Source: "handshake"}}
		}
		if strings.Contains(strings.ToLower(ver), "mariadb") {
			return []models.DetectedSoftware{{Product: "mariadb", Version: numericVersion(ver), Confidence: "probable", Source: "handshake"}}
		}
		if v := numericVersion(ver); v != "" {
			return []models.DetectedSoftware{{Product: "mysql", Version: v, Confidence: "exact", Source: "handshake"}}
		}
		return nil
	}
	line := string(b)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	if m := reSSHVersion.FindStringSubmatch(line); m != nil {
		return []models.DetectedSoftware{{Product: "openssh", Version: m[1], Confidence: "exact", Source: "banner"}}
	}
	return nil
}

// ---------- HTTP ----------

type httpProbeResult struct {
	status int
	header http.Header
	body   string
}

func httpGet(target, path string) *httpProbeResult {
	conn, err := net.DialTimeout("tcp", target, probeDialTimeout)
	if err != nil {
		return nil
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(probeIOTimeout))
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: localhost\r\nUser-Agent: health-agent-software-detect\r\nAccept: */*\r\nConnection: close\r\n\r\n", path)
	if _, err := conn.Write([]byte(req)); err != nil {
		return nil
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"})
	if err != nil {
		return nil // not HTTP
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeMaxBody))
	return &httpProbeResult{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

var (
	reApacheServer  = regexp.MustCompile(`(?i)^Apache/(\d+\.\d+\.\d+)`)
	reNginxServer   = regexp.MustCompile(`(?i)\bnginx/(\d+\.\d+\.\d+)`)
	reOpenResty     = regexp.MustCompile(`(?i)\bopenresty/(\d+(?:\.\d+){1,3})`)
	reLighttpd      = regexp.MustCompile(`(?i)\blighttpd/(\d+\.\d+\.\d+)`)
	reJettyServer   = regexp.MustCompile(`(?i)\bJetty\(?(\d+\.\d+\.\d+)`)
	reJettyBody     = regexp.MustCompile(`(?i)Powered by Jetty:// (\d+\.\d+\.\d+)`)
	reTomcatBody    = regexp.MustCompile(`Apache Tomcat/(\d+\.\d+\.\d+)`)
	reTomcatServer  = regexp.MustCompile(`(?i)\bApache Tomcat/(\d+\.\d+\.\d+)`)
	rePHPHeader     = regexp.MustCompile(`(?i)\bPHP/(\d+\.\d+\.\d+)`)
	reIISServer     = regexp.MustCompile(`(?i)Microsoft-IIS/(\d+\.\d+)`)
	reGunicorn      = regexp.MustCompile(`(?i)\bgunicorn/(\d+\.\d+\.\d+)`)
	reWerkzeugPy    = regexp.MustCompile(`(?i)\bPython/(\d+\.\d+\.\d+)`)
	reHAProxyBody   = regexp.MustCompile(`(?i)haproxy[ -]version[ :]*(\d+\.\d+(?:\.\d+)?)`)
	reHAProxyTitle  = regexp.MustCompile(`(?i)<title>\s*Statistics Report for HAProxy`)
	reWPGenerator   = regexp.MustCompile(`(?i)<meta[^>]+name=["']generator["'][^>]+content=["']WordPress (\d+\.\d+(?:\.\d+)?)`)
	reWPCoreAsset   = regexp.MustCompile(`/wp-includes/(?:js/wp-embed\.min\.js|js/wp-emoji-release\.min\.js|css/dist/block-library/style\.min\.css)\?ver=(\d+\.\d+(?:\.\d+)?)`)
	reWPFeed        = regexp.MustCompile(`<generator>https?://wordpress\.org/\?v=(\d+\.\d+(?:\.\d+)?)</generator>`)
	reRedisVersion  = regexp.MustCompile(`redis_version:(\d+\.\d+\.\d+)`)
	reValkeyVersion = regexp.MustCompile(`valkey_version:(\d+\.\d+\.\d+)`)
)

func probeHTTP(target string) []models.DetectedSoftware {
	r1 := httpGet(target, "/")
	if r1 == nil {
		return nil
	}
	// A random path gives the server's own error page (Tomcat/Jetty print their version there).
	r2 := httpGet(target, fmt.Sprintf("/health-agent-probe-%d", rand.Intn(1_000_000)))

	var out []models.DetectedSoftware
	seen := map[string]bool{}
	add := func(product, version, confidence, source string) {
		key := product
		if seen[key] {
			// keep the entry that has a version
			if version == "" {
				return
			}
			for i := range out {
				if out[i].Product == product && out[i].Version == "" {
					out[i].Version, out[i].Confidence, out[i].Source = version, confidence, source
				}
			}
			return
		}
		seen[key] = true
		out = append(out, models.DetectedSoftware{Product: product, Version: version, Confidence: confidence, Source: source})
	}

	headers := []http.Header{r1.header}
	bodies := []string{r1.body}
	if r2 != nil {
		headers = append(headers, r2.header)
		bodies = append(bodies, r2.body)
	}

	for _, h := range headers {
		parseServerHeader(h.Get("Server"), add)
		if v := rePHPHeader.FindStringSubmatch(h.Get("X-Powered-By")); v != nil {
			add("php", v[1], "probable", "http-header")
		}
	}
	for _, body := range bodies {
		if m := reTomcatBody.FindStringSubmatch(body); m != nil {
			add("tomcat", m[1], "probable", "http-body")
		}
		if m := reJettyBody.FindStringSubmatch(body); m != nil {
			add("jetty", m[1], "probable", "http-body")
		}
	}

	// HAProxy stats page.
	for _, body := range bodies {
		if reHAProxyTitle.MatchString(body) || reHAProxyBody.MatchString(body) {
			ver := ""
			if m := reHAProxyBody.FindStringSubmatch(body); m != nil {
				ver = m[1]
			}
			add("haproxy", ver, "probable", "http-body")
			break
		}
	}

	// WordPress core (base version only; plugins/themes are intentionally ignored).
	if strings.Contains(r1.body, "/wp-content/") || strings.Contains(r1.body, "/wp-includes/") || reWPGenerator.MatchString(r1.body) {
		ver, src := "", "http-body"
		if m := reWPGenerator.FindStringSubmatch(r1.body); m != nil {
			ver = m[1]
		} else if m := reWPCoreAsset.FindStringSubmatch(r1.body); m != nil {
			ver = m[1]
		} else if feed := httpGet(target, "/feed/"); feed != nil {
			if m := reWPFeed.FindStringSubmatch(feed.body); m != nil {
				ver = m[1]
			}
		}
		conf := "probable"
		if ver == "" {
			conf = "unknown"
		}
		add("wordpress", ver, conf, src)
	}

	// HashiCorp Vault publishes its version, unauthenticated, on /v1/sys/health.
	if strings.Contains(r1.header.Get("Location"), "/ui/") {
		if v := httpGet(target, "/v1/sys/health"); v != nil {
			var doc struct {
				Initialized *bool  `json:"initialized"`
				Version     string `json:"version"`
			}
			if json.Unmarshal([]byte(v.body), &doc) == nil && doc.Initialized != nil && doc.Version != "" {
				add("hashicorp-vault", numericVersion(doc.Version), "exact", "http-body")
			}
		}
	}
	return out
}

// parseServerHeader maps a Server: header value to products. add(product, version, confidence, source).
func parseServerHeader(v string, add func(product, version, confidence, source string)) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	lower := strings.ToLower(v)
	switch {
	case reTomcatServer.MatchString(v):
		add("tomcat", reTomcatServer.FindStringSubmatch(v)[1], "probable", "http-header")
	case strings.HasPrefix(lower, "apache-coyote"):
		// Tomcat connector id; Coyote/1.1 means Tomcat <= 8.0 (old), real version hidden.
		add("tomcat", "", "unknown", "http-header")
	case reJettyServer.MatchString(v):
		add("jetty", reJettyServer.FindStringSubmatch(v)[1], "probable", "http-header")
	case strings.HasPrefix(lower, "jetty"):
		add("jetty", "", "unknown", "http-header")
	case reApacheServer.MatchString(v):
		add("apache-http-server", reApacheServer.FindStringSubmatch(v)[1], "probable", "http-header")
	case lower == "apache" || strings.HasPrefix(lower, "apache "):
		add("apache-http-server", "", "unknown", "http-header")
	case reOpenResty.MatchString(v):
		add("openresty", reOpenResty.FindStringSubmatch(v)[1], "probable", "http-header")
	case reNginxServer.MatchString(v):
		add("nginx", reNginxServer.FindStringSubmatch(v)[1], "probable", "http-header")
	case lower == "nginx" || strings.HasPrefix(lower, "nginx "):
		add("nginx", "", "unknown", "http-header")
	case reLighttpd.MatchString(v):
		add("lighttpd", reLighttpd.FindStringSubmatch(v)[1], "probable", "http-header")
	case reIISServer.MatchString(v):
		add("iis", reIISServer.FindStringSubmatch(v)[1], "probable", "http-header")
	case reGunicorn.MatchString(v):
		add("gunicorn", reGunicorn.FindStringSubmatch(v)[1], "probable", "http-header")
	case strings.HasPrefix(lower, "envoy"):
		add("envoy", "", "unknown", "http-header")
	case strings.HasPrefix(lower, "caddy"):
		add("caddy", "", "unknown", "http-header")
	}
	// Modules listed with ServerTokens Full, e.g. "Apache/2.4.57 (Debian) PHP/8.2.7 OpenSSL/3.0.9"
	if m := rePHPHeader.FindStringSubmatch(v); m != nil {
		add("php", m[1], "probable", "http-header")
	}
	if strings.Contains(lower, "werkzeug") {
		if m := reWerkzeugPy.FindStringSubmatch(v); m != nil {
			add("python", m[1], "probable", "http-header")
		}
	}
}

// ---------- Redis ----------

func probeRedis(target string) []models.DetectedSoftware {
	conn, err := net.DialTimeout("tcp", target, probeDialTimeout)
	if err != nil {
		return nil
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(probeIOTimeout))
	if _, err := conn.Write([]byte("INFO server\r\n")); err != nil {
		return nil
	}
	buf := make([]byte, 4096)
	n, _ := conn.Read(buf)
	if n == 0 {
		return nil
	}
	resp := string(buf[:n])
	if m := reRedisVersion.FindStringSubmatch(resp); m != nil {
		return []models.DetectedSoftware{{Product: "redis", Version: m[1], Confidence: "exact", Source: "handshake"}}
	}
	if m := reValkeyVersion.FindStringSubmatch(resp); m != nil {
		return []models.DetectedSoftware{{Product: "valkey", Version: m[1], Confidence: "exact", Source: "handshake"}}
	}
	if strings.HasPrefix(resp, "-NOAUTH") || strings.HasPrefix(resp, "-DENIED") {
		return []models.DetectedSoftware{{Product: "redis", Version: "", Confidence: "unknown", Source: "handshake"}}
	}
	return nil
}
