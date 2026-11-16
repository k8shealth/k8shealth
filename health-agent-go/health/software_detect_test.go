package health

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fx/health-agent-go/models"
)

func collect(v string) []models.DetectedSoftware {
	var out []models.DetectedSoftware
	parseServerHeader(v, func(p, ver, c, s string) {
		out = append(out, models.DetectedSoftware{Product: p, Version: ver, Confidence: c, Source: s})
	})
	return out
}

func TestParseServerHeader(t *testing.T) {
	cases := []struct {
		header  string
		product string
		version string
	}{
		{"Apache/2.4.57 (Debian)", "apache-http-server", "2.4.57"},
		{"Apache", "apache-http-server", ""},
		{"nginx/1.24.0", "nginx", "1.24.0"},
		{"nginx", "nginx", ""},
		{"openresty/1.21.4.1", "openresty", "1.21.4.1"},
		{"Jetty(9.4.51.v20230217)", "jetty", "9.4.51"},
		{"Apache-Coyote/1.1", "tomcat", ""},
		{"Apache Tomcat/9.0.80", "tomcat", "9.0.80"},
		{"lighttpd/1.4.69", "lighttpd", "1.4.69"},
		{"Microsoft-IIS/10.0", "iis", "10.0"},
	}
	for _, c := range cases {
		got := collect(c.header)
		if len(got) == 0 || got[0].Product != c.product || got[0].Version != c.version {
			t.Errorf("%q -> %+v, want %s %q", c.header, got, c.product, c.version)
		}
	}
	// modules listed with ServerTokens Full
	got := collect("Apache/2.4.57 (Debian) PHP/8.2.7 OpenSSL/3.0.9")
	if len(got) != 2 || got[1].Product != "php" || got[1].Version != "8.2.7" {
		t.Errorf("php module not detected: %+v", got)
	}
}

func TestParseServerBannerMySQL(t *testing.T) {
	mk := func(ver string) []byte {
		b := []byte{0x4a, 0, 0, 0, 0x0a}
		b = append(b, []byte(ver)...)
		return append(b, 0, 1, 2, 3)
	}
	if r := parseServerBanner(mk("8.0.35")); len(r) != 1 || r[0].Product != "mysql" || r[0].Version != "8.0.35" {
		t.Errorf("mysql: %+v", r)
	}
	if r := parseServerBanner(mk("5.5.5-10.11.6-MariaDB-1:10.11.6+maria~ubu2204")); len(r) != 1 || r[0].Product != "mariadb" || r[0].Version != "10.11.6" {
		t.Errorf("mariadb: %+v", r)
	}
	if r := parseServerBanner([]byte("SSH-2.0-OpenSSH_9.2p1 Debian-2\r\n")); len(r) != 1 || r[0].Version != "9.2" {
		t.Errorf("ssh: %+v", r)
	}
}

func hostPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestProbeHTTPApachePHPWordPress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "Apache/2.4.57 (Debian)")
		w.Header().Set("X-Powered-By", "PHP/8.2.7")
		if r.URL.Path == "/" {
			fmt.Fprint(w, `<html><head><link rel="stylesheet" href="/wp-includes/css/dist/block-library/style.min.css?ver=6.4.2"></head><script src="/wp-content/x.js"></script></html>`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	got := probeHTTP(hostPort(t, srv))
	want := map[string]string{"apache-http-server": "2.4.57", "php": "8.2.7", "wordpress": "6.4.2"}
	for _, g := range got {
		if v, ok := want[g.Product]; ok && v == g.Version {
			delete(want, g.Product)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing detections %v in %+v", want, got)
	}
}

func TestProbeHTTPTomcatErrorPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, `<html><body><h3>Apache Tomcat/10.1.15</h3></body></html>`)
	}))
	defer srv.Close()
	got := probeHTTP(hostPort(t, srv))
	if len(got) != 1 || got[0].Product != "tomcat" || got[0].Version != "10.1.15" {
		t.Errorf("tomcat: %+v", got)
	}
}

func TestProbePortMySQLGreeting(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b := append([]byte{0x4a, 0, 0, 0, 0x0a}, []byte("8.0.35")...)
			c.Write(append(b, 0, 1, 2, 3))
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	got := probePort(listenAddr{ip: "127.0.0.1", port: port})
	if len(got) != 1 || got[0].Product != "mysql" || got[0].Port != port {
		t.Errorf("probePort: %+v", got)
	}
}
