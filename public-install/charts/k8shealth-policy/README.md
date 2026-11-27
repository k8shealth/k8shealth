# k8shealth policy

A Kyverno policy that injects the k8shealth agent sidecar into every pod labelled
`k8shealth.io/enabled: enabled`, so workloads are monitored without changing their images. The chart can also
install Kyverno itself.

It is one part of k8shealth. The [quick start](https://k8shealth.io/quickstart) installs everything together.

## Monitor a workload

Label the pod template and describe the checks in an annotation:

```yaml
metadata:
  labels:
    k8shealth.io/enabled: "enabled"
  annotations:
    k8shealth.io/health-agent-checks: |
      [{"name":"Health","kind":"http","value":{"url":"http://localhost:8080/actuator/health","statusCode":"200"}}]
```

Supported check kinds: `http`, `mysql`, `postgres`, `redis`, `mongodb`, `kafka`, `file`.

## Install

```sh
helm upgrade --install k8shealth-policy oci://ghcr.io/k8shealth/charts/k8shealth-policy \
  -n k8shealth --create-namespace
```

If Kyverno is already installed in the cluster, add `--set kyverno.install=false`.

## Related charts

| Chart | What it does |
|---|---|
| `health-manager` | The dashboard, API and incident engine |
| `k8shealth-agent-script` | ConfigMap with `health-agent.sh`, one per namespace with monitored pods |

## License

Apache License 2.0.
