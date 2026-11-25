#!/bin/sh
# -----------------------------------------------------------------------------
# k8shealth health-agent.sh
# Runs inside each monitored pod next to the k8shealth health-agent (Go) and reports
# what the pod's own filesystem and environment can see (file checks, mounts,
# datasource hints, installed software) to the agent on localhost.
#
# Part of k8shealth - Kubernetes health monitoring.  https://k8shealth.io
# Copyright (c) 2026 Myfxbook Ltd
# -----------------------------------------------------------------------------
set -eu

# If postStart shell exits and sends SIGHUP, ignore it
trap '' HUP

# -----------------------------
# Script version:
#   Local default below. CI/pre-publish: run stamp-script-version.sh to set
#   SCRIPT_VERSION="script-<12 hex>" = first 12 chars of SHA-256(agent.sh body with this line removed).
#   Same script body -> same hash. Go-only rebuilds leave agent.sh unchanged -> same SCRIPT_VERSION.
# -----------------------------
SCRIPT_VERSION="1.0.178"

# -----------------------------
# Config / defaults
# -----------------------------
TMPFILE="/tmp/script-output.json"
TMPFILES="/tmp/script-files.json"
TMPACCESS="/tmp/script-access.json"

# Run every 10s by default so file-health-check tests (run by this script) return before UI timeout
INTERVAL="${INTERVAL:-10}"
# Maximum seconds to wait for a single file read/write operation before declaring timeout
FILE_CHECK_TIMEOUT_SEC="${FILE_CHECK_TIMEOUT_SEC:-5}"
CURL_BIN="${CURL_BIN:-curl}"
# Prefer curl injected by the health-agent init container (Kyverno copies it from the agent image
# into /health-tools at pod start-up, so app images don't need curl/wget installed).
if [ -x /health-tools/curl ]; then
  CURL_BIN="/health-tools/curl"
fi
# Use wget if curl not available (e.g. alpine, minimal Java images without init-container injection)
if ! command -v "$CURL_BIN" >/dev/null 2>&1; then
  if command -v wget >/dev/null 2>&1; then
    CURL_BIN="wget"
    USE_WGET=1
  else
    # Report missing curl/wget to the agent so the error shows in the manager UI.
    # Tries bash /dev/tcp, python, or nc - at least one is usually present.
    _REPORT_TMP="/tmp/.health-err-report"
    report_missing_http_client() {
      _pod="${POD_NAME:-unknown}"
      _container="${CONTAINER_SELF_NAME:-unknown}"
      _ts="$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || echo '2000-01-01T00:00:00Z')"
      _body="{\"pod\":\"${_pod}\",\"container\":\"${_container}\",\"error\":\"No curl or wget found in container ${_container}. Install curl to enable health script.\",\"timestamp\":\"${_ts}\",\"files\":[],\"fileAccessTargets\":[]}"
      _len=${#_body}
      printf 'POST /api/script-callback HTTP/1.0\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s' "$_len" "$_body" > "$_REPORT_TMP"
      if command -v bash >/dev/null 2>&1; then
        bash -c 'cat "$1" > /dev/tcp/localhost/8082' _ "$_REPORT_TMP" 2>/dev/null || true
      elif command -v python3 >/dev/null 2>&1; then
        python3 -c "import socket;s=socket.socket();s.connect(('localhost',8082));s.sendall(open('$_REPORT_TMP','rb').read());s.close()" 2>/dev/null || true
      elif command -v python >/dev/null 2>&1; then
        python -c "import socket;s=socket.socket();s.connect(('localhost',8082));s.sendall(open('$_REPORT_TMP','rb').read());s.close()" 2>/dev/null || true
      elif command -v nc >/dev/null 2>&1; then
        nc -q 1 localhost 8082 < "$_REPORT_TMP" 2>/dev/null || true
      fi
    }
    echo "No curl or wget found - reporting to agent" >&2
    # Retry a few times so first send succeeds once agent is up (containers start in parallel)
    _retries=0
    while [ $_retries -lt 6 ]; do
      sleep 5
      report_missing_http_client
      _retries=$((_retries + 1))
    done
    while true; do
      sleep 60
      report_missing_http_client
    done
    exit 1
  fi
else
  USE_WGET=0
fi

# Check once whether the `timeout` command is available (GNU coreutils or busybox)
HAS_TIMEOUT=0
if command -v timeout >/dev/null 2>&1; then HAS_TIMEOUT=1; fi

# Required envs injected by Kyverno:
# POD_NAME, CONTAINER_SELF_NAME, AGENT_URL
POD_NAME="${POD_NAME:-}"
CONTAINER_SELF_NAME="${CONTAINER_SELF_NAME:-}"
AGENT_URL="${AGENT_URL:-}"

# Build URLs only when we have the inputs
build_urls() {
  CONTAINER="$CONTAINER_SELF_NAME"
  SCRIPT_CONFIG_URL="${AGENT_URL}/api/script-config?pod=${POD_NAME}&container=${CONTAINER}"
  POST_URL="${AGENT_URL}/api/script-callback"
}

log() {
  # log to stdout (container logs)
  printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >&2
}

get_millis() {
  # GNU date supports %3N; busybox usually doesn't -> fallback to seconds*1000
  if date +%s%3N >/dev/null 2>&1; then
    date +%s%3N
  else
    echo "$(($(date +%s) * 1000))"
  fi
}

# -----------------------------
# Helper: map UNC path -> mount path
# -----------------------------
map_network_path() {
  unc="$1"

  # If not //server/share... return as-is
  echo "$unc" | grep -Eq '^//[^/]+/[^/]+' || { echo "$unc"; return 0; }

  mounts=""

  if command -v findmnt >/dev/null 2>&1; then
    mounts="$(findmnt -rn -t cifs,smb3,smbfs -o SOURCE,TARGET 2>/dev/null || true)"
  fi

  if [ -z "$mounts" ] && [ -r /proc/mounts ]; then
    mounts="$(awk '$3 ~ /(cifs|smbfs)/ {print $1" "$2}' /proc/mounts 2>/dev/null || true)"
  fi

  [ -n "$mounts" ] || { echo "$unc"; return 0; }

  best_src=""
  best_tgt=""

  echo "$mounts" | while IFS= read -r line; do
    src=$(echo "$line" | awk '{print $1}')
    tgt=$(echo "$line" | awk '{print $2}')

    # strip trailing /
    src=$(echo "$src" | sed 's:/*$::')
    tgt=$(echo "$tgt" | sed 's:/*$::')

    case "$unc" in
      "$src"*)
        # choose longest match
        if [ "${#src}" -gt "${#best_src}" ]; then
          best_src="$src"
          best_tgt="$tgt"
        fi
      ;;
    esac
  done

  # Because of subshell in pipe above, recompute in a non-piped loop:
  best_src=""
  best_tgt=""
  echo "$mounts" | while IFS= read -r line; do
    src=$(echo "$line" | awk '{print $1}')
    tgt=$(echo "$line" | awk '{print $2}')
    src=$(echo "$src" | sed 's:/*$::')
    tgt=$(echo "$tgt" | sed 's:/*$::')

    case "$unc" in
      "$src"*)
        if [ "${#src}" -gt "${#best_src}" ]; then
          best_src="$src"
          best_tgt="$tgt"
        fi
      ;;
    esac

    # keep updating; at end write result into temp file so we can read it
    printf '%s|%s\n' "$best_src" "$best_tgt" > /tmp/.best_mount 2>/dev/null || true
  done

  if [ -r /tmp/.best_mount ]; then
    best_src=$(cut -d'|' -f1 /tmp/.best_mount 2>/dev/null || true)
    best_tgt=$(cut -d'|' -f2 /tmp/.best_mount 2>/dev/null || true)
    rm -f /tmp/.best_mount 2>/dev/null || true
  fi

  if [ -n "$best_src" ]; then
    remainder="${unc#"$best_src"}"
    remainder=$(echo "$remainder" | sed 's:^/*::')
    if [ -n "$remainder" ]; then
      echo "${best_tgt}/${remainder}"
    else
      echo "${best_tgt}"
    fi
  else
    echo "$unc"
  fi
}

# -----------------------------
# Helpers: mount source host/ip
# -----------------------------
extract_mount_source_host() {
  src="$1"
  # Typical CIFS source: //server/share
  case "$src" in
    //*)
      t="${src#//}"
      echo "$t" | awk -F'/' '{print $1}'
      return 0
      ;;
  esac
  # nfs style host:/export
  case "$src" in
    *:/*)
      h="${src%%:*}"
      [ -n "$h" ] && echo "$h" && return 0
      ;;
  esac
  echo ""
}

resolve_host_ip() {
  host="$1"
  [ -n "$host" ] || { echo ""; return 0; }
  # already IPv4
  echo "$host" | grep -Eq '^[0-9]+(\.[0-9]+){3}$' && { echo "$host"; return 0; }
  if command -v getent >/dev/null 2>&1; then
    ip="$(getent ahostsv4 "$host" 2>/dev/null | awk 'NR==1{print $1}')"
    [ -n "$ip" ] && { echo "$ip"; return 0; }
  fi
  if command -v nslookup >/dev/null 2>&1; then
    ip="$(nslookup "$host" 2>/dev/null | awk '/^Address: /{print $2}' | tail -n 1)"
    [ -n "$ip" ] && { echo "$ip"; return 0; }
  fi
  echo ""
}

# -----------------------------
# Auto-discover CIFS/SMB mounts in this container (app container has the mounts).
# Output: one line per *actual* mount (same device = same share): path|path|mode|mountSource|mountSourceIp.
# Dedupe by device; ignore inner/system mappings: exclude /mnt/* and /smb (generic mount points).
# Prefer user-facing paths (e.g. /data/app, /static); fall back to first path if only system paths.
# Deduped with config targets in main_loop so same path from sidecar + script is reported once.
# -----------------------------
list_cifs_mounts() {
  # Use /proc/mounts: device mount_point fstype options ... so we can group by device
  [ -r /proc/mounts ] || return 0
  awk '
    $3 ~ /(cifs|smbfs)/ {
      dev=$1; path=$2; opts=$4
      gsub(/\/+$/, "", path)
      # Auto-discovered mounts always use "ro" mode: the kernel rw/ro flag indicates what the
      # client *requested*, not what the app needs or the SMB server actually permits.
      # Write-access checking for specific mounts must be explicitly configured in the manager UI.
      # Using "rw" here caused false "not writable" alerts on CIFS shares where the server-side
      # ACL grants only read access to the service account (correct and expected behaviour).
      mode="ro"
      # Skip typical system/inner mount points: /mnt/* and /smb (keep /data/*, /static, etc.)
      is_system = (path ~ /^\/mnt\// || path == "/smb")
      src_host=dev
      if (dev ~ /^\/\//) { tmp=substr(dev,3); split(tmp,a,"/"); src_host=a[1] }
      else if (dev ~ /:\/.*/) { split(dev,b,":"); src_host=b[1] }
      if (!is_system) { best_path[dev]=path; best_mode[dev]=mode; best_src[dev]=src_host }
      else if (path != "/smb" && !(dev in best_path)) { best_path[dev]=path; best_mode[dev]=mode; best_src[dev]=src_host }
    }
    END { for (d in best_path) print best_path[d]"|"best_path[d]"|"best_mode[d]"|"best_src[d]"|" }
  ' /proc/mounts 2>/dev/null || true
}

# -----------------------------
# Parse config JSON (simple grep/sed style, no jq)
# -----------------------------
extract_config_files() {
  raw="$1"

  # pull "paths":[...]
  paths_blob=$(echo "$raw" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | grep -o '"paths":[[:space:]]*\[[^]]*\]' 2>/dev/null || true)
  [ -n "$paths_blob" ] || return 0

  # strip to contents
  paths=$(echo "$paths_blob" | sed 's/.*"paths":[[:space:]]*\[\(.*\)\].*/\1/' | tr -d '"' | tr ',' '\n')

  echo "$paths" | while IFS= read -r raw_path; do
    raw_path=$(echo "$raw_path" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | sed 's:/*$::')
    [ -n "$raw_path" ] || continue

    path="$(map_network_path "$raw_path")"

    if [ -f "$path" ]; then
      case "$path" in
        *.conf) echo "$path" ;;
        *.cfg)  echo "$path" ;;   # haproxy, etc.
      esac
    elif [ -d "$path" ]; then
      # *.conf and *.cfg under directory (haproxy uses .cfg)
      find "$path" -type f \( -name "*.conf" -o -name "*.cfg" \) 2>/dev/null || true
    fi
  done
}

extract_access_targets() {
  raw="$1"

  blob=$(echo "$raw" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | grep -o '"fileAccessTargets":[[:space:]]*\[[^]]*\]' 2>/dev/null || true)
  [ -n "$blob" ] || return 0

  # strip list brackets, split objects
  items=$(echo "$blob" \
    | sed 's/.*"fileAccessTargets":[[:space:]]*\[//' \
    | sed 's/\][[:space:]]*$//' \
    | sed 's/},[[:space:]]*{/\n{/g')

  echo "$items" | while IFS= read -r item; do
    raw_path=$(echo "$item" | sed -n 's/.*"path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    mode=$(echo "$item" | sed -n 's/.*"mode"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    mount_source=$(echo "$item" | sed -n 's/.*"mountSource"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    mount_source_ip=$(echo "$item" | sed -n 's/.*"mountSourceIp"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')

    [ -n "$raw_path" ] || continue
    [ -n "$mode" ] || continue

    mapped="$(map_network_path "$raw_path")"
    echo "${raw_path}|${mapped}|${mode}|${mount_source}|${mount_source_ip}"
  done
}

extract_deployment() {
  raw="$1"
  echo "$raw" | tr '\n' ' ' | sed -n 's/.*"deployment"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' || true
}

# -----------------------------
# HTTP helpers (curl or wget)
# -----------------------------
agent_get() {
  url="$1"
  timeout="${2:-2}"
  if [ "$USE_WGET" = 1 ]; then
    wget -qO- --timeout="$timeout" "${url}" 2>/dev/null || true
  else
    "$CURL_BIN" -fsS --connect-timeout 2 --max-time "$timeout" "${url}" 2>/dev/null || true
  fi
}
agent_post() {
  url="$1"
  file="$2"
  if [ "$USE_WGET" = 1 ]; then
    wget -qO- --timeout=15 --post-file="$file" --header="Content-Type: application/json" "${url}" 2>/dev/null || true
  else
    "$CURL_BIN" -fsS --connect-timeout 2 --max-time 10 -X POST -H "Content-Type: application/json" --data-binary "@$file" "${url}" 2>/dev/null || true
  fi
}

# -----------------------------
# Wait for health-agent
# -----------------------------
wait_for_agent() {
  log "Waiting for health-agent on ${AGENT_URL} ..."
  while true; do
    out="$(agent_get "${AGENT_URL}/agent" 2)"
    if [ -n "$out" ]; then
      log "health-agent is reachable"
      return 0
    fi
    sleep 1
  done
}

# -----------------------------
# Software facts: runtime / framework / server versions as seen from INSIDE this container.
# WHAT to look for is not hardcoded here: the probe rules come from the agent (GET /api/software-probes,
# header lines key=value, then one pipe-separated line per probe). The script only gathers bounded raw text
# and posts it (base64) with its normal callback; the agent turns it into product+version.
# Safety: read-only; every output is size-capped; commands only run under `timeout`; no eval; probe values are
# validated by the agent AND used only as quoted arguments here. Nothing in this block may stop the main loop:
# every step is guarded and the caller treats any failure as "no facts this round".
# Runs on the first loop and then at most every SW_INTERVAL seconds (value from the agent; 1h when the agent
# has nothing / is older), not on every iteration.
# -----------------------------
SW_INTERVAL=0
SW_LAST_TS=0
SW_MAXBYTES=4096
SW_CMD_TIMEOUT=3
SW_TOTAL_MAX=262144
SW_FIELD=""
SW_JAR=""

sw_num() {
  case "$1" in ''|*[!0-9]*) return 1 ;; esac
  # up to 10 digits: epoch seconds (date +%s) are 10 digits; callers range-check their own values
  [ "${#1}" -le 10 ]
}

# "$ENV:NAME/rest" -> value of env NAME followed by "/rest"; any other value unchanged. No eval.
sw_expand() {
  case "$1" in
    '$ENV:'*)
      _r="${1#\$ENV:}"
      _n="${_r%%/*}"
      case "$_n" in ''|*[!A-Za-z0-9_]*) return 1 ;; esac
      _v="$(printenv "$_n" 2>/dev/null || true)"
      [ -n "$_v" ] || return 1
      printf '%s%s' "$_v" "${_r#"$_n"}"
      ;;
    *) printf '%s' "$1" ;;
  esac
}

sw_probe_env() {
  printf '%s\n' "$1" | tr ',' '\n' | while IFS= read -r _n; do
    case "$_n" in ''|*[!A-Za-z0-9_]*) continue ;; esac
    _v="$(printenv "$_n" 2>/dev/null || true)"
    if [ -n "$_v" ]; then printf '%s=%s\n' "$_n" "$_v"; fi
  done
  return 0
}

# Generic variant: every "<NAME>_VERSION=<version>" variable (official images declare their software this way).
# Strict filter on purpose: only name+version-looking lines leave the container, never the rest of the environment.
sw_probe_envall() {
  env 2>/dev/null | grep -E '^[A-Z][A-Z0-9_]{1,40}_VERSION=[0-9][0-9A-Za-z._+-]{0,39}$' | head -n 64 || true
  return 0
}

# first readable file of the candidate list ("a;b;c")
sw_probe_file() {
  printf '%s\n' "$1" | tr ';' '\n' | while IFS= read -r _p; do
    _p="$(sw_expand "$_p" 2>/dev/null || true)"
    [ -n "$_p" ] || continue
    if [ -f "$_p" ] && [ -r "$_p" ]; then
      head -c "$SW_MAXBYTES" "$_p" 2>/dev/null || true
      break
    fi
  done
  return 0
}

# "<first existing binary> <one dash flag>", only when `timeout` exists (never run a binary without a limit)
sw_probe_cmd() {
  [ "$HAS_TIMEOUT" = 1 ] || return 0
  case "$2" in -*) ;; *) return 0 ;; esac
  case "$2" in *[!A-Za-z-]*) return 0 ;; esac
  printf '%s\n' "$1" | tr ';' '\n' | while IFS= read -r _b; do
    _b="$(sw_expand "$_b" 2>/dev/null || true)"
    [ -n "$_b" ] || continue
    case "$_b" in
      */*) [ -x "$_b" ] || continue ;;
      *) command -v "$_b" >/dev/null 2>&1 || continue ;;
    esac
    timeout "$SW_CMD_TIMEOUT" "$_b" "$2" 2>&1 </dev/null | head -c "$SW_MAXBYTES" || true
    break
  done
  return 0
}

# entry of / entry listing of the app's main jar
sw_probe_jar() {
  [ -n "$SW_JAR" ] || return 0
  command -v unzip >/dev/null 2>&1 || return 0
  case "$2" in ''|*[!A-Za-z0-9_./*-]*) return 0 ;; esac
  _t=""
  if [ "$HAS_TIMEOUT" = 1 ]; then _t="timeout $SW_CMD_TIMEOUT"; fi
  case "$1" in
    read) $_t unzip -p "$SW_JAR" "$2" 2>/dev/null </dev/null | head -c "$SW_MAXBYTES" || true ;;
    list) $_t unzip -l "$SW_JAR" 2>/dev/null </dev/null | awk '{print $NF}' | grep -F -- "$2" | head -c "$SW_MAXBYTES" || true ;;
  esac
  return 0
}

# main jar of PID 1 (same lookup as detect_spring_datasource); empty when the app is not a jar
sw_find_jar() {
  SW_JAR=""
  [ -r /proc/1/cmdline ] || return 0
  _line="$(tr '\0' ' ' < /proc/1/cmdline 2>/dev/null || true)"
  _prev=""
  set -f
  for _w in $_line; do
    if [ "$_prev" = "-jar" ]; then SW_JAR="$_w"; break; fi
    _prev="$_w"
  done
  set +f
  if [ -z "$SW_JAR" ]; then
    SW_JAR="$(printf '%s' "$_line" | grep -o '[^ ]*\.jar' | grep -vi 'javaagent' | head -1 || true)"
  fi
  case "$SW_JAR" in
    '') return 0 ;;
    /*) ;;
    *) if [ -d /proc/1/cwd ]; then SW_JAR="/proc/1/cwd/$SW_JAR"; fi ;;
  esac
  if [ ! -f "$SW_JAR" ]; then SW_JAR=""; fi
  return 0
}

# Fetch the rules, run them, set SW_FIELD to ',"softwareFacts":[...]' (or "" = nothing to report).
sw_run() {
  SW_FIELD=""
  command -v base64 >/dev/null 2>&1 || return 0
  _spec="$(agent_get "${AGENT_URL}/api/software-probes" 5 2>/dev/null || true)"
  if [ -z "$_spec" ]; then SW_INTERVAL=3600; return 0; fi
  SW_INTERVAL=21600
  _probes=""
  _cnt=0
  while IFS= read -r _l; do
    case "$_l" in
      interval=*) _v="${_l#interval=}"; if sw_num "$_v" && [ "$_v" -ge 300 ]; then SW_INTERVAL="$_v"; fi ;;
      maxbytes=*) _v="${_l#maxbytes=}"; if sw_num "$_v" && [ "$_v" -ge 512 ] && [ "$_v" -le 65536 ]; then SW_MAXBYTES="$_v"; fi ;;
      timeout=*) _v="${_l#timeout=}"; if sw_num "$_v" && [ "$_v" -ge 1 ] && [ "$_v" -le 30 ]; then SW_CMD_TIMEOUT="$_v"; fi ;;
      maxtotal=*) _v="${_l#maxtotal=}"; if sw_num "$_v" && [ "$_v" -ge 8192 ] && [ "$_v" -le 4194304 ]; then SW_TOTAL_MAX="$_v"; fi ;;
      env\|*|file\|*|cmd\|*|jar\|*)
        _cnt=$((_cnt + 1))
        if [ "$_cnt" -le 64 ]; then _probes="${_probes}${_l}
"; fi
        ;;
    esac
  done <<SWEOF
$_spec
SWEOF
  if [ -z "$_probes" ]; then return 0; fi
  sw_find_jar
  _json=""
  _total=0
  while IFS='|' read -r _k _id _a _b _c; do
    case "$_id" in ''|*[!A-Za-z0-9._-]*) continue ;; esac
    _out=""
    case "$_k" in
      env)  _out="$(sw_probe_env "$_a" 2>/dev/null </dev/null | head -c "$SW_MAXBYTES" || true)" ;;
      envall) _out="$(sw_probe_envall 2>/dev/null </dev/null | head -c "$SW_MAXBYTES" || true)" ;;
      file) _out="$(sw_probe_file "$_a" 2>/dev/null </dev/null | head -c "$SW_MAXBYTES" || true)" ;;
      cmd)  _out="$(sw_probe_cmd "$_a" "$_b" 2>/dev/null </dev/null | head -c "$SW_MAXBYTES" || true)" ;;
      jar)  _out="$(sw_probe_jar "$_a" "$_b" 2>/dev/null </dev/null | head -c "$SW_MAXBYTES" || true)" ;;
      *) continue ;;
    esac
    [ -n "$_out" ] || continue
    _enc="$(printf '%s' "$_out" | base64 2>/dev/null | tr -d '\n\r ' || true)"
    [ -n "$_enc" ] || continue
    _total=$((_total + ${#_enc}))
    [ "$_total" -le "$SW_TOTAL_MAX" ] || break
    _item="{\"id\":\"${_id}\",\"b64\":\"${_enc}\"}"
    if [ -n "$_json" ]; then _json="${_json},${_item}"; else _json="$_item"; fi
  done <<SWEOF2
$_probes
SWEOF2
  SW_FIELD=",\"softwareFacts\":[${_json}]"
  return 0
}

# -----------------------------
# Auto-detect Spring Boot datasource from application JAR
# Outputs a JSON object string or empty string when nothing is found.
# Requires: unzip (common in JDK/JRE images), /proc/1/cmdline readable.
# Skips Jasypt-encrypted (ENC(...)) and unresolved placeholder (${...}) values.
# -----------------------------
detect_spring_datasource() {
  command -v unzip >/dev/null 2>&1 || return 0

  # Find the Spring Boot fat JAR: prefer the path after `-jar` (not `-javaagent:...jar`, which often appears first)
  jar_path=""
  if [ -r /proc/1/cmdline ]; then
    _line=$(tr '\0' ' ' < /proc/1/cmdline 2>/dev/null || true)
    _prev=""
    for _w in $_line; do
      if [ "$_prev" = "-jar" ]; then
        jar_path="$_w"
        break
      fi
      _prev="$_w"
    done
    if [ -z "$jar_path" ]; then
      jar_path=$(printf '%s' "$_line" | grep -o '[^ ]*\.jar' | grep -vi 'javaagent' | head -1 || true)
    fi
    if [ -z "$jar_path" ]; then
      jar_path=$(printf '%s' "$_line" | grep -o '[^ ]*\.jar' | head -1 || true)
    fi
  fi
  [ -n "$jar_path" ] && [ -f "$jar_path" ] || return 0

  # Active profile (use first if multiple are listed)
  profile=$(printenv SPRING_PROFILES_ACTIVE 2>/dev/null | tr ',' '\n' | grep -v '^[[:space:]]*$' | head -1 || true)

  # Try profile-specific properties first, then fall back to application.properties
  props=""
  used_profile=""
  if [ -n "$profile" ]; then
    props=$(unzip -p "$jar_path" "BOOT-INF/classes/application-${profile}.properties" 2>/dev/null || true)
    [ -n "$props" ] && used_profile="$profile"
  fi
  if [ -z "$props" ]; then
    props=$(unzip -p "$jar_path" "BOOT-INF/classes/application.properties" 2>/dev/null || true)
    used_profile=""
  fi
  [ -n "$props" ] || return 0

  ds_url=$(printf '%s\n' "$props" | grep '^spring\.datasource\.url=' | head -1 | cut -d= -f2- | tr -d '\r' || true)
  ds_user=$(printf '%s\n' "$props" | grep '^spring\.datasource\.username=' | head -1 | cut -d= -f2- | tr -d '\r' || true)
  ds_pass=$(printf '%s\n' "$props" | grep '^spring\.datasource\.password=' | head -1 | cut -d= -f2- | tr -d '\r' || true)

  # Skip missing / encrypted / unresolved-placeholder URLs
  [ -n "$ds_url" ] || return 0
  case "$ds_url" in
    ENC\(*)  return 0 ;;
    \${*)    return 0 ;;
  esac

  # Escape double-quotes and backslashes so the JSON is valid
  esc() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

  props_b64=""
  if command -v base64 >/dev/null 2>&1; then
    props_b64=$(printf '%s' "$props" | head -c "${CONF_READ_MAX:-1048576}" 2>/dev/null | base64 2>/dev/null | tr -d '\n' || true)
  fi

  printf '{"url":"%s","username":"%s","password":"%s","profile":"%s","propertiesBase64":"%s"}' \
    "$(esc "$ds_url")" "$(esc "$ds_user")" "$(esc "$ds_pass")" "$(esc "$used_profile")" "$props_b64"
}

# -----------------------------
# Dedicated volume usage (PVC / block-device mounts) for disk-space alerts
# -----------------------------
# Cheap by design: one statfs per mount via `df -P -k <mountpoint>` (never du, never a directory walk),
# at most VOL_MAX mounts, each wrapped in `timeout` when available, re-collected at most every
# VOL_INTERVAL seconds (the cached JSON is re-sent in between). POSIX sh + awk + df only.
# Real block-device mounts and network filesystems (cifs/smb3/nfs/nfs4/ceph/glusterfs) are reported; the
# manager de-duplicates shared network volumes across pods by source host + size.
# Block-device mounts only below the node-root rule: The container root, emptyDir and hostPath on the node disk
# share the device behind /etc/hosts (always a node-root bind mount) and are skipped: their usage is the
# node disk, which the node-disk alert already covers.
VOL_INTERVAL="${VOL_INTERVAL:-60}"
VOL_MAX="${VOL_MAX:-10}"
VOL_LAST_TS=0
VOL_JSON="[]"

collect_volumes() {
  [ -r /proc/mounts ] || { printf '[]'; return 0; }
  command -v df >/dev/null 2>&1 || { printf '[]'; return 0; }
  _vol_root_dev="$(awk '$2=="/etc/hosts"{print $1; exit}' /proc/mounts 2>/dev/null || true)"
  _vol_list="$(awk -v rootdev="$_vol_root_dev" -v max="$VOL_MAX" '
    ($1 ~ /^\/dev\// && $3 ~ /^(ext2|ext3|ext4|xfs|btrfs|f2fs|jfs|zfs|reiserfs)$/) || ($3 ~ /^(cifs|smb3|nfs|nfs4|ceph|glusterfs|fuse\.glusterfs)$/ && $1 !~ /^\/dev\//) {
      if ($1 == rootdev) next
      if ($1 ~ /\\|"|\|/) next
      if ($2 ~ /\\|"|\|/) next
      if ($2 ~ /^\/(proc|sys|dev)(\/|$)/) next
      if ($2 ~ /^\/etc\/(hosts|hostname|resolv\.conf)$/) next
      if ($2 == "/dev/termination-log") next
      if (seen[$1]++) next
      if (n >= max) next
      n++
      print $2 "|" $3 "|" $1
    }' /proc/mounts 2>/dev/null || true)"
  [ -n "$_vol_list" ] || { printf '[]'; return 0; }
  _vol_out=""
  _vol_first=1
  _vol_tmp="/tmp/.vol-list.$$"
  printf '%s\n' "$_vol_list" > "$_vol_tmp"
  while IFS='|' read -r _vp _vfs _vsrc; do
    [ -n "$_vp" ] || continue
    if [ "$HAS_TIMEOUT" = 1 ]; then
      _vdf="$(timeout 3 df -P -k "$_vp" 2>/dev/null | awk 'NR==2{print $2 " " $3}')" || _vdf=""
    else
      _vdf="$(df -P -k "$_vp" 2>/dev/null | awk 'NR==2{print $2 " " $3}')" || _vdf=""
    fi
    _vtotal="${_vdf%% *}"
    _vused="${_vdf#* }"
    case "$_vtotal$_vused" in *[!0-9]*|"") continue ;; esac
    [ "$_vtotal" -gt 0 ] 2>/dev/null || continue
    if [ "$_vol_first" -ne 1 ]; then _vol_out="$_vol_out,"; fi
    _vol_first=0
    _vol_out="$_vol_out{\"path\":\"$_vp\",\"fs\":\"$_vfs\",\"src\":\"$_vsrc\",\"totalKb\":$_vtotal,\"usedKb\":$_vused}"
  done < "$_vol_tmp"
  rm -f "$_vol_tmp" 2>/dev/null || true
  printf '[%s]' "$_vol_out"
}

# The loop runs every INTERVAL seconds (60 in the injected policy). A file test queued in the manager UI adds its path to
# the agent's fileAccessTargets, so instead of sleeping the whole interval this wakes as soon as that target list changes
# (the agent picks the test up within ~3s), and the UI answer comes back in seconds, not up to a minute. The list is
# compared sorted: the agent does not promise an order. A failed or empty fetch never wakes the loop.
wait_next_run() {
  _prev="$1"
  _waited=0
  while [ "$_waited" -lt "$INTERVAL" ]; do
    sleep 3
    _waited=$((_waited + 3))
    _cfg="$(agent_get "$SCRIPT_CONFIG_URL" 2)"
    [ -n "$_cfg" ] || continue
    _now="$(extract_access_targets "$_cfg" 2>/dev/null | sort || true)"
    if [ "$_now" != "$_prev" ]; then return 0; fi
  done
}
# -----------------------------
# Main
# -----------------------------
main_loop() {
  while true; do
    # Validate envs
    if [ -z "$POD_NAME" ] || [ -z "$CONTAINER_SELF_NAME" ] || [ -z "$AGENT_URL" ]; then
      log "Missing env(s). POD_NAME='$POD_NAME' CONTAINER_SELF_NAME='$CONTAINER_SELF_NAME' AGENT_URL='$AGENT_URL'. Retrying..."
      sleep 2
      continue
    fi

    build_urls

    RAW="$(agent_get "$SCRIPT_CONFIG_URL" 10)"

    if [ -z "$RAW" ]; then
      log "health-agent not ready / empty response. retrying in 2s..."
      sleep 2
      continue
    fi

    FILES="$(extract_config_files "$RAW" || true)"
    TARGETS="$(extract_access_targets "$RAW" || true)"
    CONFIG_TARGETS="$(printf '%s\n' "$TARGETS" | sort)"
    # Auto-discover CIFS/SMB mounts in this container; merge with config targets, dedupe by path so
    # same mount is not reported twice if sidecar (CifsMounts) and script both see it
    AUTO_MOUNTS="$(list_cifs_mounts 2>/dev/null || true)"
    if [ -n "$AUTO_MOUNTS" ]; then
      COMBINED="$(echo "$TARGETS"; echo "$AUTO_MOUNTS")"
      TARGETS="$(echo "$COMBINED" | awk -F'|' 'NF>=2 && $2!="" {p=$2} !seen[p]++ {print}')"
    fi
    DEPLOYMENT="$(extract_deployment "$RAW")"

    # ---------------- files[]
    echo "[" > "$TMPFILES"
    first=1
    echo "$FILES" | while IFS= read -r path; do
      [ -n "$path" ] || continue
      if [ ! -f "$path" ]; then
        if [ ! -e "$path" ]; then
          log "File check: managed config path missing: $path"
        else
          log "File check: managed config path is not a regular file: $path"
        fi
      fi

      content=""
      if [ -f "$path" ]; then
        if command -v base64 >/dev/null 2>&1; then
          # Large vhosts (many Alias/VirtualHost) exceed 51KB; truncated config = missing mounts in UI
          content="$(head -c "${CONF_READ_MAX:-1048576}" "$path" 2>/dev/null | base64 2>/dev/null | tr -d '\n' || true)"
        fi
      fi

      if [ "$first" -ne 1 ]; then echo "," >> "$TMPFILES"; fi
      first=0

      {
        echo "{"
        echo "\"path\": \"${path}\","
        echo "\"type\": \"file\","
        echo "\"mode\": \"rw\","
        echo "\"base64\": \"${content}\""
        echo "}"
      } >> "$TMPFILES"
    done
    echo "]" >> "$TMPFILES"

    # ---------------- fileAccessTargets[]
    echo "[" > "$TMPACCESS"
    firsta=1

    echo "$TARGETS" | while IFS='|' read -r path_raw path_mapped mode mount_source mount_source_ip; do
      [ -n "$path_raw" ] || continue

      readable=false
      writable=false
      timed_out=false
      readDurationMs=0
      writeDurationMs=0

      test_path="$path_mapped"

      # ---- read check ----
      # All stat/access/io calls are wrapped in a single timeout subshell so that
      # a blocking CIFS/NFS stat() (e.g. during SMB session reconnection) is caught
      # by the timeout rather than freezing the whole script loop indefinitely.
      # start_read is set BEFORE the block so readDurationMs includes stat() blocking time.
      _path_type=""
      start_read="$(get_millis)"
      if [ "$HAS_TIMEOUT" = 1 ]; then
        _read_out=$(timeout "$FILE_CHECK_TIMEOUT_SEC" sh -c '
          p="$1"
          [ -e "$p" ] || { echo "missing"; exit 0; }
          [ -r "$p" ] || { echo "noperm";  exit 0; }
          if [ -f "$p" ]; then
            head -c 1 "$p" >/dev/null 2>&1 && echo "file:ok" || echo "file:err"
          elif [ -d "$p" ]; then
            # stat() the directory only — avoids QUERY_DIRECTORY over SMB which can
            # block on huge directories. Readability of the dir itself is confirmed
            # by the fact that stat() returned without error.
            echo "dir:ok"
          else
            echo "other:ok"
          fi
        ' _ "$test_path" 2>/dev/null); _rc=$?
        if [ $_rc -eq 124 ]; then
          timed_out=true
        else
          case "$_read_out" in
            *:ok)   readable=true ;;
          esac
          case "$_read_out" in
            file:*) _path_type=file ;;
            dir:*)  _path_type=dir  ;;
            other:*)_path_type=other;;
          esac
        fi
      else
        # No timeout binary: run without protection (best-effort)
        if [ -e "$test_path" ] && [ -r "$test_path" ]; then
          if [ -f "$test_path" ]; then
            _path_type=file
            head -c 1 "$test_path" >/dev/null 2>&1 && readable=true
          elif [ -d "$test_path" ]; then
            _path_type=dir
            readable=true
          else
            _path_type=other
            readable=true
          fi
        fi
      fi
      end_read="$(get_millis)"
      readDurationMs=$((end_read - start_read))

      # ---- write check (rw mode only, skipped if read already timed out) ----
      # mktemp and the [ -d ]/[ -w ] permission checks are inside the timeout
      # subshell so a slow/hung SMB write path is caught rather than blocking.
      if [ "$mode" = "rw" ] && [ "$timed_out" != true ]; then
        parent_dir="$test_path"
        if [ "${_path_type:-}" = "file" ]; then
          parent_dir=$(dirname "$test_path")
        fi

        start_write="$(get_millis)"
        if [ "$HAS_TIMEOUT" = 1 ]; then
          _write_out=$(timeout "$FILE_CHECK_TIMEOUT_SEC" sh -c '
            d="$1"
            [ -d "$d" ] || { echo "nodir";   exit 0; }
            [ -w "$d" ] || { echo "noperm";  exit 0; }
            tf=$(mktemp "$d/.writecheck.XXXXXX" 2>/dev/null) || { echo "mkfail"; exit 0; }
            printf "test" > "$tf" 2>/dev/null && echo "ok" || echo "err"
            rm -f "$tf" 2>/dev/null || true
          ' _ "$parent_dir" 2>/dev/null); _rc=$?
          if [ $_rc -eq 124 ]; then
            timed_out=true
          elif [ "$_write_out" = "ok" ]; then
            writable=true
          fi
        else
          if [ -d "$parent_dir" ] && [ -w "$parent_dir" ]; then
            TMPTEST="$(mktemp "${parent_dir}/.writecheck.XXXXXX" 2>/dev/null || true)"
            if [ -n "$TMPTEST" ]; then
              echo "test" > "$TMPTEST" 2>/dev/null && writable=true || true
              rm -f "$TMPTEST" 2>/dev/null || true
            fi
          fi
        fi
        end_write="$(get_millis)"
        writeDurationMs=$((end_write - start_write))
      fi

      checkedAt="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
      if [ -z "${mount_source_ip:-}" ] && [ -n "${mount_source:-}" ]; then
        mount_source_ip="$(resolve_host_ip "$mount_source")"
      fi

      access_ok=true
      if [ "$timed_out" = true ]; then
        access_ok=false
      else
        case "$(printf '%s' "$mode" | tr '[:upper:]' '[:lower:]')" in
          rw)
            if [ "$readable" != true ] || [ "$writable" != true ]; then access_ok=false; fi
            ;;
          *)
            if [ "$readable" != true ] || [ "$writable" != false ]; then access_ok=false; fi
            ;;
        esac
      fi
      if [ "$access_ok" != true ]; then
        log "File check failed: path=${path_raw} resolved=${path_mapped} mode=${mode:-ro} readable=${readable} writable=${writable} timed_out=${timed_out}"
      fi

      if [ "$firsta" -ne 1 ]; then echo "," >> "$TMPACCESS"; fi
      firsta=0

      {
        echo "{"
        echo "\"path\": \"${path_raw}\","
        echo "\"resolvedPath\": \"${path_mapped}\","
        echo "\"type\": \"fileAccessTarget\","
        echo "\"mode\": \"${mode}\","
        echo "\"mountSource\": \"${mount_source:-}\","
        echo "\"mountSourceIp\": \"${mount_source_ip:-}\","
        echo "\"readable\": ${readable},"
        echo "\"writable\": ${writable},"
        echo "\"timedOut\": ${timed_out},"
        echo "\"readDurationMs\": ${readDurationMs},"
        echo "\"writeDurationMs\": ${writeDurationMs},"
        echo "\"checkedAt\": \"${checkedAt}\","
        echo "\"base64\": \"\""
        echo "}"
      } >> "$TMPACCESS"
    done

    echo "]" >> "$TMPACCESS"

    # ---------------- Get handles and threads (main workload = PID 1, not this shell)
    HANDLES=0
    THREADS=0

    if [ -d /proc/1/fd ]; then
        HANDLES=$(ls -1 /proc/1/fd 2>/dev/null | wc -l || echo "0")
    elif [ -d /proc/self/fd ]; then
        HANDLES=$(ls -1 /proc/self/fd 2>/dev/null | wc -l || echo "0")
    fi

    if [ -r /proc/1/status ]; then
        THREADS=$(grep "^Threads:" /proc/1/status 2>/dev/null | awk '{print $2}' || echo "0")
    elif [ -r /proc/self/status ]; then
        THREADS=$(grep "^Threads:" /proc/self/status 2>/dev/null | awk '{print $2}' || echo "0")
    fi

    # Ensure we have numeric values
    HANDLES=${HANDLES:-0}
    THREADS=${THREADS:-0}

    # ---------------- Auto-detect Spring Boot datasource (once per loop; cheap when JAR absent)
    SPRING_DS_JSON="$(detect_spring_datasource 2>/dev/null || true)"
    if [ -n "$SPRING_DS_JSON" ]; then
      SPRING_DS_ARR="[$SPRING_DS_JSON]"
    else
      SPRING_DS_ARR="[]"
    fi

    # ---------------- Software facts (first loop, then every SW_INTERVAL s; rules come from the agent; never fatal)
    SW_FIELD=""
    _sw_now="$(date +%s 2>/dev/null || echo 0)"
    if sw_num "$_sw_now" && [ "$_sw_now" -gt 0 ] && [ $((_sw_now - SW_LAST_TS)) -ge "$SW_INTERVAL" ]; then
      sw_run >/dev/null 2>&1 || SW_FIELD=""
      SW_LAST_TS="$_sw_now"
    fi

    # ---------------- Dedicated volume usage (cached; re-collected every VOL_INTERVAL s; never fatal)
    _vol_now="$(date +%s 2>/dev/null || echo 0)"
    if sw_num "$_vol_now" && [ "$_vol_now" -gt 0 ] && [ $((_vol_now - VOL_LAST_TS)) -ge "$VOL_INTERVAL" ]; then
      VOL_JSON="$(collect_volumes 2>/dev/null || printf '[]')"
      [ -n "$VOL_JSON" ] || VOL_JSON="[]"
      VOL_LAST_TS="$_vol_now"
    fi

    # ---------------- final payload
    cat > "$TMPFILE" <<EOF
{
  "files": $(cat "$TMPFILES"),
  "fileAccessTargets": $(cat "$TMPACCESS"),
  "pod": "$POD_NAME",
  "container": "$CONTAINER_SELF_NAME",
  "deployment": "$DEPLOYMENT",
  "scriptVersion": "$SCRIPT_VERSION",
  "timestamp": "$(date -u '+%Y-%m-%dT%H:%M:%SZ')",
  "handles": $HANDLES,
  "threads": $THREADS,
  "volumes": $VOL_JSON,
  "springDatasources": $SPRING_DS_ARR$SW_FIELD
}
EOF

    agent_post "$POST_URL" "$TMPFILE" >/dev/null 2>&1 || log "POST failed, will retry next interval"

    rm -f "$TMPFILE" "$TMPFILES" "$TMPACCESS" 2>/dev/null || true

    wait_next_run "$CONFIG_TARGETS"
  done
}

log "k8shealth health-agent.sh ${SCRIPT_VERSION} - https://k8shealth.io"

# Ensure agent is reachable first (but don't crash forever if env missing)
if [ -n "$AGENT_URL" ]; then
  wait_for_agent
fi

main_loop
