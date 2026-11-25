{{- define "k8shealth.policyName" -}}
inject-health-agent-{{ regexReplaceAll "[^a-z0-9]+" (regexFind ":[^:/@]+$" .Values.healthAgentSidecar.image | trimPrefix ":" | default "latest" | lower) "-" | trunc 40 | trimSuffix "-" }}
{{- end }}
{{- define "k8shealth.policy" -}}
# Unified health-agent injection ClusterPolicy.
# Domain label/annotation prefix is driven by values.yaml:
#   healthAgentSidecar.domain  e.g. k8shealth.io
# VERSIONING: bump health-agent/policy-version below whenever this file changes.
# Local render example:
#   helm template kyverno . -f values.yaml -f values-<cluster>.yaml --show-only templates/health-agent-script-policy.yaml | kubectl apply -f -
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: {{ include "k8shealth.policyName" . }}
  labels:
    app.kubernetes.io/part-of: k8shealth
  annotations:
{{- if .Values.healthAgentSidecar.pruneLast }}
    # Policy name changes with every agent version. With PruneLast Argo creates the new policy first and
    # prunes the old one last, so pods admitted in between still get the health agent (no injection gap).
    argocd.argoproj.io/sync-options: PruneLast=true
{{- end }}
    health-agent/policy-version: {{ .Values.healthAgentSidecar.image | regexFind "[0-9]+\\.[0-9]+\\.[0-9]+" | quote }}
spec:
  validationFailureAction: enforce
  background: true
  rules:

    # ------------------------------------------------------------
    # 0) Copy the health-agent.sh ConfigMap into the pod's namespace. A ConfigMap volume can only come from the
    #    pod's own namespace, so without this any namespace other than the one holding the ConfigMap would get
    #    pods that cannot start. synchronize is false on purpose: deleting the first pod must not delete the copy
    #    that other pods in the namespace still use.
    # ------------------------------------------------------------
    - name: copy-health-script-configmap
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      generate:
        apiVersion: v1
        kind: ConfigMap
        name: health-agent-health-script
        namespace: {{ `{{ request.object.metadata.namespace }}` | quote }}
        synchronize: false
        clone:
          namespace: {{ .Values.healthAgentSidecar.scriptNamespace | quote }}
          name: health-agent-health-script

    # ------------------------------------------------------------
    # 1) Add ConfigMap volume with health-agent.sh
    # ------------------------------------------------------------
    - name: add-health-script-volume
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      mutate:
        patchStrategicMerge:
          spec:
            volumes:
              - name: health-agent-script
                configMap:
                  name: health-agent-health-script
                  defaultMode: 493
                  # A missing script must never keep a pod from starting.
                  optional: true

    # ------------------------------------------------------------
    # 1b) Add health-tools emptyDir volume (shared between init container and app containers)
    #     The init container copies curl here; app containers read it via /health-tools/curl.
    # ------------------------------------------------------------
    - name: add-health-tools-volume
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      mutate:
        patchStrategicMerge:
          spec:
            volumes:
              - name: health-tools
                emptyDir: {}

    # ------------------------------------------------------------
    # 1c) Inject zz-health-tools init container — copies curl from the health-agent
    #     image into the health-tools emptyDir so app containers have curl available
    #     even if their base image does not include it.
    # ------------------------------------------------------------
    - name: inject-health-tools-init-container
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      mutate:
        patchStrategicMerge:
          spec:
            initContainers:
              - name: zz-health-tools
                image: {{ .Values.healthAgentSidecar.image | quote }}
                imagePullPolicy: IfNotPresent
                command:
                  - sh
                  - -c
                  - |
                    if [ ! -f /usr/bin/curl ]; then
                      echo "health-tools: curl not in agent image, skipping injection"
                      exit 0
                    fi
                    cp /usr/bin/curl /health-tools/.curl-bin
                    for lib in \
                      /usr/lib/libcurl* /lib/libz* \
                      /usr/lib/libnghttp2* /usr/lib/libidn2* /usr/lib/libpsl* \
                      /lib/libssl* /lib/libcrypto* \
                      /usr/lib/libbrotli* /usr/lib/libunistring*; do
                      [ -f "$lib" ] && cp "$lib" /health-tools/ || true
                    done
                    cp /lib/ld-musl-x86_64.so.1 /health-tools/ 2>/dev/null || true
                    printf '#!/bin/sh\nexec /health-tools/ld-musl-x86_64.so.1 --library-path /health-tools /health-tools/.curl-bin "$@"\n' \
                      > /health-tools/curl
                    chmod +x /health-tools/.curl-bin /health-tools/curl
                    echo "health-tools: curl injected ok"
                    # Inject tini as subreaper wrapper for health-agent.sh.
                    # /sbin/tini is a statically-linked Alpine binary; it has no shared-lib
                    # dependencies so a plain copy is enough — no ld-musl wrapper needed.
                    if [ -f /sbin/tini ]; then
                      cp /sbin/tini /health-tools/tini
                      chmod +x /health-tools/tini
                      echo "health-tools: tini injected ok"
                    else
                      echo "health-tools: tini not found in agent image, skipping"
                    fi
                resources:
                  requests:
                    cpu: "10m"
                    memory: "16Mi"
                  limits:
                    cpu: "100m"
                    memory: "32Mi"
                volumeMounts:
                  - name: health-tools
                    mountPath: /health-tools

    # ------------------------------------------------------------
    # 1d) Mount health-tools into ALL app containers (read-only)
    # ------------------------------------------------------------
    - name: inject-health-tools-mount
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      mutate:
        foreach:
          - list: "request.object.spec.containers"
            preconditions:
              all:
                - key: {{ `{{ element.name }}` | quote }}
                  operator: NotEquals
                  value: zz-health-agent
            patchStrategicMerge:
              spec:
                containers:
                  - name: {{ `{{ element.name }}` | quote }}
                    volumeMounts:
                      - name: health-tools
                        mountPath: /health-tools
                        readOnly: true

    # ------------------------------------------------------------
    # 2) Mount script into ALL containers
    # ------------------------------------------------------------
    - name: inject-health-script-mount
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      mutate:
        foreach:
          - list: "request.object.spec.containers"
            patchStrategicMerge:
              spec:
                containers:
                  - name: {{ `{{ element.name }}` | quote }}
                    volumeMounts:
                      - name: health-agent-script
                        mountPath: /opt/health-tools/health-agent.sh
                        subPath: health-agent.sh
                        readOnly: true

    # ------------------------------------------------------------
    # 3) Inject env vars into APP containers (NOT sidecar)
    # ------------------------------------------------------------
    - name: inject-health-script-env
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      mutate:
        foreach:
          - list: "request.object.spec.containers"
            preconditions:
              all:
                - key: {{ `{{ element.name }}` | quote }}
                  operator: NotEquals
                  value: zz-health-agent
            patchStrategicMerge:
              spec:
                containers:
                  - name: {{ `{{ element.name }}` | quote }}
                    env:
                      - name: POD_NAME
                        valueFrom:
                          fieldRef:
                            fieldPath: metadata.name
                      - name: CONTAINER_SELF_NAME
                        value: {{ `{{ element.name }}` | quote }}
                      - name: AGENT_URL
                        value: "http://localhost:8082"
                      - name: INTERVAL
                        value: "60"
                      - name: CURL_BIN
                        value: "curl"

    # ------------------------------------------------------------
    # 4) START SCRIPT inside ORIGINAL app container (postStart)
    # ------------------------------------------------------------
    - name: start-health-script-poststart
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      mutate:
        foreach:
          - list: "request.object.spec.containers"
            preconditions:
              all:
                - key: {{ `{{ element.name }}` | quote }}
                  operator: NotEquals
                  value: zz-health-agent
            patchStrategicMerge:
              spec:
                containers:
                  - name: {{ `{{ element.name }}` | quote }}
                    lifecycle:
                      postStart:
                        exec:
                          command:
                            - /bin/sh
                            - -c
                            - |
                              echo "starting health-agent script"
                              if [ -x /health-tools/tini ] && /health-tools/tini -- true 2>/dev/null; then
                                nohup /health-tools/tini -s -g -- sh -c 'while true; do sh /opt/health-tools/health-agent.sh >>/tmp/health-agent.log 2>&1; echo "health-agent.sh exited, restarting in 5s" >>/tmp/health-agent.log; sleep 5; done' >/dev/null 2>&1 &
                              else
                                nohup sh -c 'while true; do sh /opt/health-tools/health-agent.sh >>/tmp/health-agent.log 2>&1; echo "health-agent.sh exited, restarting in 5s" >>/tmp/health-agent.log; sleep 5; done' >/dev/null 2>&1 &
                              fi

    # ------------------------------------------------------------
    # 5) Inject health-agent sidecar
    # patchesJson6902 appends the container so the workload stays containers[0].
    # HEALTH_AGENT_MANAGER_URL / HEALTH_AGENT_HC_CHECKS_JSON (annotation lookups) are set in rule 5a below
    # because patchesJson6902 value blocks do not support the || fallback operator.
    # ------------------------------------------------------------
    - name: inject-health-agent-sidecar
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      preconditions:
        all:
          # Avoid duplicate sidecar on updates / re-admission
          - key: {{ `{{ request.object.spec.containers[?name == 'zz-health-agent'] | length(@) }}` | quote }}
            operator: Equals
            value: "0"
      mutate:
        patchesJson6902: |-
          - op: add
            path: /spec/containers/-
            value:
              name: zz-health-agent
              image: {{ .Values.healthAgentSidecar.image | quote }}
              imagePullPolicy: IfNotPresent
              resources:
                requests:
                  cpu: "10m"
                  memory: "32Mi"
                limits:
                  cpu: "500m"
                  memory: "128Mi"
              ports:
                - name: agent-http
                  containerPort: 8082
              env:
                - name: POD_NAME
                  valueFrom:
                    fieldRef:
                      fieldPath: metadata.name
                - name: NAMESPACE
                  valueFrom:
                    fieldRef:
                      fieldPath: metadata.namespace
                - name: CONTAINER
                  value: ""
                - name: AGENT_HTTP_PORT
                  value: "8082"
                - name: AGENT_HTTP_HEALTH_PATH
                  value: "/health"
                - name: HEALTH_AGENT_LISTEN_PORT
                  value: "8082"
                - name: HEALTH_AGENT_MANAGER_URL
                  value: ""
                - name: HEALTH_AGENT_HC_CHECKS_JSON
                  value: "[]"

    # ------------------------------------------------------------
    # 5a) Patch dynamic env vars onto the sidecar via patchStrategicMerge.
    # All three need JMESPath / || fallback which patchesJson6902 does NOT support.
    # ------------------------------------------------------------
    - name: inject-health-agent-sidecar-urls
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      mutate:
        patchStrategicMerge:
          spec:
            containers:
              - name: zz-health-agent
                env:
                  - name: CONTAINER
                    value: {{ `{{ request.object.spec.containers[?name != 'zz-health-agent'] | [0].name }}` | quote }}
                  - name: HEALTH_AGENT_MANAGER_URL
                    value: {{ printf "{{ request.object.metadata.annotations.\"%s/health-agent-manager-url\" || '%s' }}" .Values.healthAgentSidecar.domain .Values.healthAgentSidecar.managerUrl | quote }}
                  - name: MANAGER_URL
                    value: {{ printf "{{ request.object.metadata.annotations.\"%s/health-agent-manager-url\" || '%s' }}" .Values.healthAgentSidecar.domain .Values.healthAgentSidecar.managerUrl | quote }}
                  - name: HEALTH_AGENT_HC_CHECKS_JSON
                    value: {{ printf "{{ request.object.metadata.annotations.\"%s/health-agent-checks\" || '[]' }}" .Values.healthAgentSidecar.domain | quote }}
                  - name: HC_CHECKS_JSON
                    value: {{ printf "{{ request.object.metadata.annotations.\"%s/health-agent-checks\" || '[]' }}" .Values.healthAgentSidecar.domain | quote }}

    # ------------------------------------------------------------
    # 5a2) Mirror literal env entries from app container → sidecar.
    # [?value != null] skips valueFrom/envFrom entries (no .value field) so the sidecar's own
    # envFrom Secret is never shadowed by an empty string.
    # Agent-specific settings use HEALTH_AGENT_* (rule 5 / 5a) so app literals (e.g. SERVER_PORT) cannot clash.
    # ------------------------------------------------------------
    - name: mirror-literal-env-to-sidecar
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      mutate:
        foreach:
          - list: "request.object.spec.containers[?name != 'zz-health-agent'] | [0].env[?value != null]"
            patchStrategicMerge:
              spec:
                containers:
                  - name: zz-health-agent
                    env:
                      - name: {{ `{{ element.name }}` | quote }}
                        value: {{ `{{ element.value }}` | quote }}

    # ------------------------------------------------------------
    # 5b) Inject envFrom into health-agent sidecar
    # ------------------------------------------------------------
    - name: inject-health-agent-envfrom
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      preconditions:
        all:
          - key: {{ printf "{{ request.object.metadata.annotations.\"%s/health-agent-envfrom-secret\" || '' }}" .Values.healthAgentSidecar.domain | quote }}
            operator: NotEquals
            value: ""
      mutate:
        patchStrategicMerge:
          spec:
            containers:
              - name: zz-health-agent
                envFrom:
                  - secretRef:
                      name: {{ printf "{{ request.object.metadata.annotations.\"%s/health-agent-envfrom-secret\" }}" .Values.healthAgentSidecar.domain | quote }}

    # ------------------------------------------------------------
    # 5c) Inject optional second envFrom into health-agent sidecar
    # NOTE: patchStrategicMerge replaces the entire envFrom list (no merge key),
    # so this rule must include BOTH secrets to avoid dropping the first one.
    # ------------------------------------------------------------
    - name: inject-health-agent-envfrom-2
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      preconditions:
        all:
          - key: {{ printf "{{ request.object.metadata.annotations.\"%s/health-agent-envfrom-secret-2\" || '' }}" .Values.healthAgentSidecar.domain | quote }}
            operator: NotEquals
            value: ""
      mutate:
        patchStrategicMerge:
          spec:
            containers:
              - name: zz-health-agent
                envFrom:
                  - secretRef:
                      name: {{ printf "{{ request.object.metadata.annotations.\"%s/health-agent-envfrom-secret\" }}" .Values.healthAgentSidecar.domain | quote }}
                  - secretRef:
                      name: {{ printf "{{ request.object.metadata.annotations.\"%s/health-agent-envfrom-secret-2\" }}" .Values.healthAgentSidecar.domain | quote }}

    # ------------------------------------------------------------
    # 5d) Keep zz-health-agent last in spec.containers.
    # patchStrategicMerge updates that only name the sidecar cause the apiserver
    # to reorder the list so zz-health-agent becomes containers[0]. Tools such
    # as Argo CD then show the agent image as the primary container. JSON Patch
    # "move" restores app-first ordering when needed.
    # ------------------------------------------------------------
    - name: ensure-health-agent-container-last
      match:
        any:
          - resources:
              kinds: ["Pod"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      preconditions:
        all:
          - key: {{ `{{ request.object.spec.containers[0].name }}` | quote }}
            operator: Equals
            value: zz-health-agent
          - key: {{ `{{ request.object.spec.containers | length(@) }}` | quote }}
            operator: GreaterThan
            value: "1"
      mutate:
        patchesJson6902: |-
          - op: move
            from: /spec/containers/0
            path: /spec/containers/-

    # ------------------------------------------------------------
    # 6) Inject Service port (only if Service opts in)
    # ------------------------------------------------------------
    - name: inject-health-agent-service-port
      match:
        any:
          - resources:
              kinds: ["Service"]
              selector:
                matchExpressions:
                  - key: {{ printf "%s/enabled" .Values.healthAgentSidecar.domain }}
                    operator: In
                    values: ["enabled","true","on","1"]
      mutate:
        patchStrategicMerge:
          spec:
            ports:
              - name: agent
                port: 8082
                protocol: TCP
                targetPort: agent-http
{{- end }}
