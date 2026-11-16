package models

import "time"

// DetectedConnection represents a single remote IP:port observed in /proc/net/tcp.
// The manager resolves these to service names to detect uncovered dependencies.
type DetectedConnection struct {
	RemoteIP   string `json:"remoteIp"`
	RemotePort int    `json:"remotePort"`
	// LocalPort is the pod-side port of this connection.  Used by the manager to classify
	// inbound vs. outbound connections (inbound = we are the server; outbound = we are the client).
	// 0 means the agent did not send it (older agent version) — manager treats as outbound for safety.
	LocalPort  int    `json:"localPort,omitempty"`
	FirstSeen  int64  `json:"firstSeen"` // unix epoch seconds
	LastSeen   int64  `json:"lastSeen"`  // unix epoch seconds
}

// DetectedSoftware is one infrastructure product + base version fingerprinted on a pod's listening port
// (Apache httpd, nginx, Tomcat, Jetty, HAProxy, MySQL, Redis, WordPress core, ...).
// Product uses endoflife.date slugs where one exists so the manager can compare against release data.
type DetectedSoftware struct {
	Port       int    `json:"port"`
	Product    string `json:"product"`
	Version    string `json:"version"`    // empty = product recognised but version hidden
	Confidence string `json:"confidence"` // exact | probable | unknown
	Source     string `json:"source"`     // handshake | banner | http-header | http-body | container-*
	Container  string `json:"container,omitempty"` // set when reported from inside an app container (port is then 0)
}

// AgentPayload matches Java AgentPayload (for /api/sync and manager callback).
type AgentPayload struct {
	PodName            string                              `json:"podName"`
	DeploymentName     string                              `json:"deploymentName"`
	Version            string                              `json:"version"`
    MemoryUsageBytes   int64                               `json:"memoryUsageBytes,omitempty"`
	// ManagerRttMs is the TCP 3-way-handshake round-trip time (milliseconds) to the
	// health-manager host, measured by the agent immediately before each callback POST.
	// Uses net.DialTimeout so it captures only pure network latency with zero server-side
	// processing overhead.  Zero means "not yet measured" (first payload after startup or
	// TCP dial failed).
	ManagerRttMs       int64                               `json:"managerRttMs,omitempty"`
	ScriptPayloads     map[string]map[string]ScriptPayload  `json:"scriptPayloads"`
	HealthBaseChecks   map[string]map[string][]HealthCheck  `json:"healthBaseChecks"`   // pod -> container -> slice (JSON array)
	ManagerHealthChecks []JsonCheck                        `json:"managerHealthChecks,omitempty"` // checks pulled from manager DB (GET deployment-health-checks/yaml); file checks appear in script-config and get run by script
    ClusterObject     *ClusterObject                      `json:"clusterObject,omitempty"`     // canonical pod list from manager; synced to peers so /status shows same list on every agent
	// DetectedConnections is the 24-hour union of observed remote IP:port pairs from /proc/net/tcp.
	// The manager resolves these IPs to service names and surfaces uncovered dependencies in the health score.
	DetectedConnections []DetectedConnection               `json:"detectedConnections,omitempty"`
	// DetectedSoftware is the latest fingerprint of infrastructure software listening in this pod
	// (rescanned every few hours). The manager compares it to release/EOL data to flag outdated software.
	DetectedSoftware   []DetectedSoftware                  `json:"detectedSoftware,omitempty"`
	// ActionLogs accumulates action invocation results since the last successful callback.
	// The manager persists these to check_action_log so they appear in the Actions tab history.
	// Drained (cleared) by the agent after each successful callback POST.
	ActionLogs         []ActionLogEntry                    `json:"actionLogs,omitempty"`
}

// ActionLogEntry records a single action URL invocation by the agent.
// Sent in AgentPayload.ActionLogs so the manager can persist it to check_action_log.
type ActionLogEntry struct {
	DeploymentName string `json:"deploymentName"`
	CheckName      string `json:"checkName"`
	Event          string `json:"event"`      // "open" | "close"
	FiredAt        int64  `json:"firedAt"`    // Unix ms
	HttpStatus     int    `json:"httpStatus"` // 0 = network error
	DurationMs     int    `json:"durationMs"`
	Success        bool   `json:"success"`
	ErrorMessage   string `json:"errorMessage,omitempty"`
	ResponseSnippet string `json:"responseSnippet,omitempty"`
}

// ScriptPayload matches Java ScriptPayload.
type ScriptPayload struct {
	Pod               string              `json:"pod"`
	Deployment        string              `json:"deployment"`
	Container         string              `json:"container"`
	// ScriptVersion is the version of the agent.sh script that produced this payload (SCRIPT_VERSION constant in agent.sh).
	ScriptVersion     string              `json:"scriptVersion,omitempty"`
	Files             []ConfigFile        `json:"files"`
	FileAccessTargets []FileAccessTarget  `json:"fileAccessTargets"`
	Timestamp         time.Time           `json:"timestamp"`
	Handles           *int                `json:"handles"`
	Threads           *int                `json:"threads"`
	// Volumes lists dedicated block-device mounts (PVCs) with usage, collected by health-agent.sh via df.
	Volumes           []ScriptVolume      `json:"volumes"`
	Error             string              `json:"error,omitempty"`
	// Auto-detected Spring Boot datasources (populated by health-agent.sh by scanning application-{profile}.properties inside the JAR)
	SpringDatasources []SpringDatasource `json:"springDatasources,omitempty"`
}

// ScriptVolume is one dedicated volume (mount path, filesystem, size and used KiB) reported by health-agent.sh.
type ScriptVolume struct {
	Path    string `json:"path"`
	Fs      string `json:"fs,omitempty"`
	Src     string `json:"src,omitempty"`
	TotalKb int64  `json:"totalKb"`
	UsedKb  int64  `json:"usedKb"`
}

// SpringDatasource holds a datasource auto-detected from a Spring Boot properties file inside the application JAR.
type SpringDatasource struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	// Profile is the Spring profile the config was loaded from (empty = application.properties fallback)
	Profile string `json:"profile"`
	// Full application*.properties file from the JAR (base64), for manager Configs tab / parity with filesystem configs
	PropertiesBase64 string `json:"propertiesBase64,omitempty"`
}

// ConfigFile matches ScriptPayload.ConfigFile.
type ConfigFile struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Base64 string `json:"base64"`
}

// FileAccessTarget matches Java FileAccessTarget.
type FileAccessTarget struct {
	Path            string    `json:"path"`
	Mode            string    `json:"mode"`
	Readable        bool      `json:"readable"`
	Writable        bool      `json:"writable"`
	TimedOut        bool      `json:"timedOut,omitempty"`
	ReadDurationMs  int       `json:"readDurationMs"`
	WriteDurationMs int       `json:"writeDurationMs"`
	CheckedAt       time.Time `json:"checkedAt"`
	MountSource     string    `json:"mountSource,omitempty"`
	MountSourceIP   string    `json:"mountSourceIp,omitempty"`
}

// ScriptConfig response for GET /api/script-config (paths + fileAccessTargets).
type ScriptConfig struct {
	Paths             []string            `json:"paths"`
	FileAccessTargets []FileAccessTarget   `json:"fileAccessTargets"`
}

// HealthCheck is the common + type-specific check (JSON type discriminator "type": "http"|"mysql"|"file").
type HealthCheck struct {
	Type     string    `json:"type"`
	Name     string    `json:"name"`
	Passed   bool      `json:"passed"`
	Duration int       `json:"duration"`
	Time     time.Time `json:"time"`
	// CheckStartedAt is the wall-clock time the check began executing (before the network call).
	// Used by the manager to set incidentStart accurately (= start of first failing attempt,
	// not the moment the result arrived), giving correct downtime in notifications.
	CheckStartedAt time.Time `json:"checkStartedAt,omitempty"`
	// HTTP
	URL          string `json:"url,omitempty"`
	Port         int    `json:"port,omitempty"`
	Response     string `json:"response,omitempty"`
	ResponseCode string `json:"responseCode,omitempty"`
	ExpectBody   string `json:"expectBody,omitempty"`
	// Responder names the layer that produced an HTTP error response (e.g. "nginx", "HAProxy", "Cloudflare").
	// Empty when the check passed, got no response, or the layer was not recognised.
	Responder string `json:"responder,omitempty"`
	// TimeoutMs / HTTPMethod are the effective settings the agent ran the check with, reported so the
	// manager UI can show YAML-defined values. Omitted when empty (older managers simply ignore them).
	TimeoutMs  int    `json:"timeoutMs,omitempty"`
	HTTPMethod string `json:"httpMethod,omitempty"`
	// Redis / Postgres / MongoDB — host:port identity
	Host string `json:"host,omitempty"`
	// MySQL
	User string `json:"user,omitempty"`
	Db   string `json:"db,omitempty"`
	Pass string `json:"pass,omitempty"`
	// File
	Path          string `json:"path,omitempty"`
	Mode          string `json:"mode,omitempty"`
	ConfigFile    string `json:"configFile,omitempty"`
	Message       string `json:"message,omitempty"`
	MountSource   string `json:"mountSource,omitempty"`
	MountSourceIP string `json:"mountSourceIp,omitempty"`
}

// HealthRow for /status HTML (one row in the table).
type HealthRow struct {
	Pod        string
	Deployment string
	Passed     bool
	Inactive   bool
	Type       string
	Name       string
	FilePath   string
	URL        string
	Host       string
	Port       string
	Database   string
	Response   string
	Code       string
	TimeAgo    string
}

// ClusterObject is the manager callback response (optional).
type ClusterObject struct {
	Name         string                    `json:"name"`
	Deployments  map[string]DeploymentObj  `json:"deployments"`
}

// DeploymentObj minimal for ClusterObject (manager callback response). Pods keyed by pod name so /status can show same list on every agent.
type DeploymentObj struct {
	Name string                 `json:"name"`
	Pods map[string]interface{} `json:"pods"`
}

// HC checks config: HC_CHECKS_JSON is a JSON array of JsonCheck.
type JsonCheck struct {
	Name  string       `json:"name"`
	Kind  string       `json:"kind"`
	Value *JsonCheckValue `json:"value"`
}

type JsonCheckValue struct {
	URL        interface{} `json:"url"`   // string or { "fromEnv": "VAR" } / { "value": "..." }
	Method     string      `json:"method,omitempty"` // HTTP method: GET (default), HEAD, POST
	Host       interface{} `json:"host"`
	Db         interface{} `json:"db"`
	User       interface{} `json:"user"`
	Pass       interface{} `json:"pass"`
	Path       interface{} `json:"path"`
	Mode       string      `json:"mode"`
	Description string     `json:"description"`
	// Deprecated: use Contains instead. Kept for backward compatibility.
	Result     string      `json:"result"`
	// Contains checks that the response body contains this string (case-insensitive). Supersedes Result when both are set.
	Contains   string      `json:"contains"`
	// NotContains checks that the response body does NOT contain this string (case-insensitive). Fails if the string is found.
	NotContains string     `json:"notContains,omitempty"`
	// StatusCode checks that the HTTP response status code equals this value (e.g. "200", "403").
	// When empty, any 2xx status is considered passing.
	StatusCode string      `json:"statusCode"`
	// TimeoutMs sets the HTTP request timeout in milliseconds. 0 means use the default (5000ms).
	TimeoutMs   int        `json:"timeoutMs,omitempty"`
	// IntervalSecs overrides the global check cadence for this specific check.
	// 0 means use the global default (5s). Minimum enforced value is 5s.
	IntervalSecs int       `json:"intervalSecs,omitempty"`
	// Action fields — fired by the agent from inside the pod when a check changes state.
	// ActionWaitForIncident: when false (default), fire on the very first failure even if
	// no incident has opened yet (pass→fail transition — "on detection").
	// When true, fire only after 2 consecutive failures, mirroring the manager's
	// incident-open threshold ("on confirmed incident").
	ActionWaitForIncident bool `json:"actionWaitForIncident,omitempty"`
	// ActionUrl is the URL to call. ActionMethod defaults to POST. ActionHeaders is a map of
	// header name → value pairs (e.g. {"Authorization": "Bearer ..."}).
	// ActionOnClose: also fire when the check recovers (transitions failing → passing).
	ActionUrl     string            `json:"actionUrl,omitempty"`
	ActionMethod  string            `json:"actionMethod,omitempty"`
	ActionHeaders map[string]string `json:"actionHeaders,omitempty"`
	ActionOnClose bool              `json:"actionOnClose,omitempty"`
}
