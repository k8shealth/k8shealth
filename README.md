# k8shealth

Kubernetes health monitoring: a lightweight sidecar agent that runs health checks inside your pods, plus Helm charts and an installer for the k8shealth manager.

- `health-agent-go/` - the open-source agent (Go).
- `public-install/` - installer, Helm charts and a demo workload. Start with [public-install/README.md](public-install/README.md).

The k8shealth manager is distributed as a container image (`ghcr.io/k8shealth/health-manager`) under the proprietary [Manager EULA](MANAGER-EULA.md); it is not open source. The agent, charts and scripts in this repository are licensed under the Apache License 2.0, see [LICENSE](LICENSE).

Copyright 2026 Myfxbook Ltd.