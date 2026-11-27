# k8shealth manager

The k8shealth manager is the dashboard, API and incident engine that runs inside your Kubernetes cluster. It
collects the results of the health checks run by the k8shealth agent sidecar (HTTP, MySQL, PostgreSQL, Redis,
MongoDB, Kafka, file checks), opens an incident only after repeated failures, and notifies your team.

## Install

The easiest way is the installer, which sets up the manager, the agent script and the injection policy together:
see the [quick start](https://k8shealth.io/quickstart).

To install the manager chart on its own:

```sh
helm upgrade --install k8shealth oci://ghcr.io/k8shealth/charts/health-manager \
  -n k8shealth --create-namespace \
  --set mysql.enabled=true --set onboarding.enabled=true
```

`mysql.enabled=true` deploys a small MySQL next to the manager. To use your own database instead, set
`externalDatabase.url`, `externalDatabase.username` and `externalDatabase.password`.

Then open the dashboard:

```sh
kubectl -n k8shealth port-forward svc/health-manager 8080:80
```

The setup code printed by `helm install` is single use and valid for 24 hours.

## Related charts

| Chart | What it does |
|---|---|
| `k8shealth-agent-script` | ConfigMap with `health-agent.sh`, one per namespace with monitored pods |
| `k8shealth-policy` | Kyverno policy that injects the agent sidecar into labelled pods |

## Requirements

Kubernetes with `kubectl` and Helm 3.8 or newer (OCI charts).

## License

The charts are licensed under Apache License 2.0. The manager image is distributed separately under its own terms.
