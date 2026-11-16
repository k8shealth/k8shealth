package health

import (
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestBuildMySQLDSNRoundTripsSpecialChars(t *testing.T) {
	dsn, err := buildMySQLDSN("user", "p@ss:/w?rd&x=1", "db.local:3306", "appdb", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", dsn, err)
	}
	if cfg.User != "user" || cfg.Passwd != "p@ss:/w?rd&x=1" || cfg.Addr != "db.local:3306" || cfg.DBName != "appdb" {
		t.Fatalf("round trip mismatch: %+v", cfg)
	}
}

func TestBuildMySQLDSNRejectsParamInjection(t *testing.T) {
	if _, err := buildMySQLDSN("u", "p", "h:3306", "x?allowAllFiles=true", time.Second, time.Second); err == nil {
		t.Fatal("db name with '?' must be rejected")
	}
	if _, err := buildMySQLDSN("u", "p", "h:3306)/x?allowAllFiles=true", "", time.Second, time.Second); err == nil {
		t.Fatal("host with ')' must be rejected")
	}
}

func TestMysqlURLToDSNParts(t *testing.T) {
	dsn, err := mysqlURLToDSN("jdbc:mysql://host1/app?useSSL=false", "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "host1:3306" || cfg.DBName != "app" || cfg.User != "u" || cfg.Passwd != "p" {
		t.Fatalf("unexpected: %+v", cfg)
	}
	if cfg.Timeout != 5*time.Second || cfg.ReadTimeout != 8*time.Second {
		t.Fatalf("timeouts: %+v", cfg)
	}
}
