# health-agent-go

Go implementation of the health agent, API-compatible with the Java agent so you can run it side-by-side and compare behavior.

## Environment (same as Java)

| Variable        | Default (if unset) | Description                    |
|-----------------|--------------------|--------------------------------|
| `POD_NAME`      | localhost          | Current pod name               |
| `DEPLOYMENT`    | (from pod name)    | Deployment name                |
| `NAMESPACE`     | default            | Kubernetes namespace           |
| `POD_IP`        | 127.0.0.1          | Own IP (for agent sync)        |
| `HEALTH_AGENT_MANAGER_URL` | (falls back to `MANAGER_URL`) | Manager base URL (preferred in sidecars so app `MANAGER_URL` does not collide) |
| `MANAGER_URL`   | http://health-manager.infrastructure.svc.cluster.local:8080 | Legacy manager base URL |
| `HEALTH_AGENT_LISTEN_PORT` | (falls back to `SERVER_PORT`, else 8082) | HTTP listen port (preferred; avoids app `SERVER_PORT` e.g. Node `80`) |
| `SERVER_PORT`   | 8082               | Legacy HTTP server port |
| `HEALTH_AGENT_HC_CHECKS_JSON` | (falls back to `HC_CHECKS_JSON`) | JSON array of health checks |
| `HC_CHECKS_JSON`| (empty)            | Legacy JSON health checks      |

## Port and API

- Listens on **8082** (same as Java).
- Endpoints: `GET /status`, `GET /agent`, `GET /script`, `GET /cluster`, `GET /api/script-config`, `POST /api/script-callback`, `POST /api/sync`, `POST /api/set-pod-name`, `GET /api/fake-config`, `POST /api/test-add-path`.

## Run locally

```bash
cd health-agent-go
go build -o health-agent-go .
./health-agent-go
```

With env (example):

```bash
POD_NAME=my-pod DEPLOYMENT=my-deploy MANAGER_URL=http://localhost:8080 ./health-agent-go
```

## Run with Docker (compare to Java)

```bash
docker build -t health-agent-go .
docker run --rm -p 8082:8082 \
  -e POD_NAME=go-agent \
  -e MANAGER_URL=http://host.docker.internal:8080 \
  health-agent-go
```

Run the Java agent on another port (e.g. 8083) to compare:

- Go: `http://localhost:8082/status`, `http://localhost:8082/agent`
- Java: `http://localhost:8083/status`, `http://localhost:8083/agent`

## Build version (same logic as Java agent)

Version is baked in at build time (like Java agent’s `agent.version` in application.properties). Jenkins must pass **`--build-arg VERSION=$IMAGE_TAG`** when building the image so the Agents List shows the same version as the image tag (e.g. `1.39`). The QA pipeline does this via `buildAndPushDockerImage(..., buildArgs: [VERSION: env.IMAGE_TAG])`; if your shared library uses a different parameter name, pass the build-arg there.

## Kyverno sidecar

Same port (8082), same env vars, and same API contract as the Java agent, so existing Kyverno sidecar injection that targets the Java agent works with this binary without changes.
