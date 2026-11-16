# k8shealth - install files

Everything needed to run the k8shealth manager and agent in your own Kubernetes cluster.
Container images and Helm charts are published to GitHub Container Registry (`ghcr.io/k8shealth`).

| Path | What |
|---|---|
| `charts/health-manager` | The manager (dashboard + API), with an optional bundled MySQL |
| `charts/k8shealth-agent-script` | ConfigMap holding `health-agent.sh`, cloned into each namespace that has monitored pods |
| `charts/k8shealth-policy` | Kyverno policy that injects the agent sidecar into pods labelled `k8shealth.io/enabled: enabled` (optionally bundles Kyverno) |
| `demo/demo-workloads.yaml` | Three small demo apps with checks, one failing on purpose |
| `install.sh` | One-command install of all of the above |

## Quick start

```sh
./install.sh
kubectl -n k8shealth port-forward svc/health-manager 8080:80   # then open http://localhost:8080
kubectl apply -f demo/demo-workloads.yaml
```

Requires `kubectl` and `helm` (3.8+) pointed at the cluster. The full step-by-step guide, including
cloud-specific setup (EKS, AKS, GKE, bare metal), is at https://k8shealth.io/quickstart.

## Monitor your own pods

Label the pod template and describe the checks in an annotation:

```yaml
metadata:
  labels:
    k8shealth.io/enabled: "enabled"
  annotations:
    k8shealth.io/health-agent-checks: |
      [{"name":"Health","kind":"http","value":{"url":"http://localhost:8080/actuator/health","statusCode":"200"}}]
```

Supported check kinds: `http`, `mysql`, `file`.

## Install options

`install.sh` reads these environment variables (all optional):

- `K8SH_MANAGER_IMAGE`, `K8SH_AGENT_IMAGE` - use your own mirror of the images
- `K8SH_MANAGER_CHART` - path to a different manager chart

## License

Apache License 2.0 - see [LICENSE](LICENSE). The k8shealth manager image is distributed separately under its own terms.
