package health

import (
	"encoding/json"
	"testing"

	"github.com/fx/health-agent-go/models"
)

func TestMergeSpringMysqlCheck_FillsIncompleteManagerCheck(t *testing.T) {
	// Typical case: manager DB check "spring-db-prod" with host=name, no URL, timeout 1000.
	checks := []ResolvedCheck{
		{Name: "dashboard-backend-http", Kind: "http", URL: "http://127.0.0.1:8763/x"},
		{Name: "spring-db-prod", Kind: "mysql", Host: "spring-db-prod", TimeoutMs: 1000, FromDB: true},
	}
	sc := ResolvedCheck{Name: "spring-db-prod", Kind: "mysql", URL: "jdbc:mysql://192.0.2.4:6446/affiliate", User: "dashboard", Pass: "x"}
	out := mergeSpringMysqlCheck(checks, sc)
	if len(out) != 2 {
		t.Fatalf("expected 2 checks (no duplicate name), got %d", len(out))
	}
	got := out[1]
	if got.URL != sc.URL || got.User != "dashboard" || got.Pass != "x" {
		t.Fatalf("connection info not filled from spring properties: %+v", got)
	}
	if got.TimeoutMs != 1000 || !got.FromDB {
		t.Fatalf("override settings lost: %+v", got)
	}
}

func TestMergeSpringMysqlCheck_SameEndpointInheritsCredentials(t *testing.T) {
	// UI override after the form split the JDBC URL: host:port + db present, user/pass empty.
	checks := []ResolvedCheck{{Name: "spring-db-prod", Kind: "mysql", URL: "mysql://10.1.2.3:3306/affiliate", FromDB: true}}
	out := mergeSpringMysqlCheck(checks, ResolvedCheck{Name: "spring-db-prod", Kind: "mysql", URL: "jdbc:mysql://10.1.2.3:3306/affiliate", User: "u", Pass: "p"})
	if len(out) != 1 || out[0].User != "u" || out[0].Pass != "p" || out[0].URL != "mysql://10.1.2.3:3306/affiliate" {
		t.Fatalf("credentials not inherited: %+v", out)
	}
}

func TestMergeSpringMysqlCheck_ExplicitCheckWins(t *testing.T) {
	checks := []ResolvedCheck{{Name: "spring-db-prod", Kind: "mysql", URL: "mysql://custom:3306/db", User: "u", Pass: "p"}}
	out := mergeSpringMysqlCheck(checks, ResolvedCheck{Name: "spring-db-prod", Kind: "mysql", URL: "jdbc:mysql://other/db"})
	if len(out) != 1 || out[0].URL != "mysql://custom:3306/db" {
		t.Fatalf("explicit check should win: %+v", out)
	}
}

func TestMergeSpringMysqlCheck_AppendsWhenAbsent(t *testing.T) {
	out := mergeSpringMysqlCheck(nil, ResolvedCheck{Name: "spring-db", Kind: "mysql", URL: "jdbc:mysql://h/db"})
	if len(out) != 1 {
		t.Fatalf("expected append, got %d", len(out))
	}
}
// Exact payload the manager served locally after overriding an auto-detected MySQL check from the UI
// (host = JDBC URL, db empty, pass empty). Without the merge this resolves to a URL-less check.
func TestManagerOverrideOfDetectedMysql_ResolvesToRunnableCheck(t *testing.T) {
	raw := `[{"kind":"mysql","name":"spring-db-prod","value":{"pass":"","host":"jdbc:mysql://10.1.2.3:3306/affiliate","description":"","user":"dashboard","db":"","timeoutMs":1000}}]`
	var jcs []models.JsonCheck
	if err := json.Unmarshal([]byte(raw), &jcs); err != nil {
		t.Fatal(err)
	}
	checks := ResolveManagerChecks(jcs, map[string]string{})
	if len(checks) != 1 || checks[0].URL != "" {
		t.Fatalf("precondition: override alone must be URL-less, got %+v", checks)
	}
	spring := ResolvedCheck{Name: "spring-db-prod", Kind: "mysql", URL: "jdbc:mysql://10.1.2.3:3306/affiliate", User: "dashboard", Pass: "secret"}
	out := mergeSpringMysqlCheck(checks, spring)
	if len(out) != 1 || out[0].URL != spring.URL || out[0].Pass != "secret" || out[0].TimeoutMs != 1000 {
		t.Fatalf("override not overlaid on detected datasource: %+v", out)
	}
}