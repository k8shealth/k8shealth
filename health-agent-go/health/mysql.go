package health

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/fx/health-agent-go/models"
	"github.com/fx/health-agent-go/registry"
)

// runMySQLCheck connects to a MySQL/MariaDB database and pings it.
func runMySQLCheck(pod, container string, c ResolvedCheck, reg *registry.Registry) bool {
	url := c.URL
	if url == "" {
		return false
	}
	// URL may be "mysql://host:port/db" or "jdbc:mysql://host:port/db" - convert to DSN
	dsn, err := mysqlURLToDSN(url, c.User, c.Pass)
	start := time.Now()
	var db *sql.DB
	if err == nil {
		db, err = sql.Open("mysql", dsn)
	}
	var passed bool
	var message string
	if err != nil {
		message = "Open: " + err.Error()
	} else {
		defer db.Close()
		db.SetConnMaxLifetime(5 * time.Second)
		err = db.Ping()
		if err != nil {
			message = "Ping: " + err.Error()
		} else {
			passed = true
			message = "Database connection successful"
		}
	}
	duration := int(time.Since(start).Milliseconds())
	h := models.HealthCheck{
		Type:           "mysql",
		Name:           c.Name,
		Passed:         passed,
		Duration:       duration,
		Time:           time.Now(),
		CheckStartedAt: start,
		URL:            url,
		User:           c.User,
		Db:             c.Db,
		Response:       message,
	}
	reg.AddHealthCheck(pod, container, "mysql", h)
	return passed
}

// mysqlURLToDSN converts mysql://host:port/db or jdbc:mysql://host:port/db to go-sql-driver DSN: user:pass@tcp(host:port)/db
func mysqlURLToDSN(urlStr, user, pass string) (string, error) {
	urlStr = strings.TrimPrefix(urlStr, "jdbc:mysql://")
	urlStr = strings.TrimPrefix(urlStr, "mysql://")
	hostPort := urlStr
	dbName := ""
	if i := strings.Index(urlStr, "/"); i >= 0 {
		hostPort = urlStr[:i]
		if i+1 < len(urlStr) {
			dbPart := urlStr[i+1:]
			if j := strings.Index(dbPart, "?"); j >= 0 {
				dbName = dbPart[:j]
			} else {
				dbName = dbPart
			}
		}
	}
	if hostPort == "" {
		hostPort = "127.0.0.1:3306"
	} else if !strings.Contains(hostPort, ":") {
		hostPort = hostPort + ":3306"
	}
	return buildMySQLDSN(user, pass, hostPort, dbName, 8*time.Second, 5*time.Second)
}

// buildMySQLDSN builds a DSN via the driver's own formatter so credentials, host or database
// values containing ':', '@', '/' or '?' cannot be misparsed or inject extra DSN parameters
// (e.g. allowAllFiles / allowCleartextPasswords).
func buildMySQLDSN(user, pass, hostPort, db string, readTimeout, writeTimeout time.Duration) (string, error) {
	// The driver does not escape the database name, so reject characters that would end it early.
	if strings.ContainsAny(user, ":\x00") || strings.ContainsAny(db, "?/\x00") || strings.ContainsAny(hostPort, "()?/\x00 ") {
		return "", errors.New("invalid characters in MySQL host or database name")
	}
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = pass
	cfg.Net = "tcp"
	cfg.Addr = hostPort
	cfg.DBName = db
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = readTimeout
	cfg.WriteTimeout = writeTimeout
	return cfg.FormatDSN(), nil
}

