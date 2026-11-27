# k8shealth agent script

A ConfigMap holding `health-agent.sh`, the script the k8shealth agent sidecar runs for script-based checks. It
has to exist in every namespace that has monitored pods, so install this chart once per such namespace.

It is one part of k8shealth. The [quick start](https://k8shealth.io/quickstart) installs everything together; use
this chart on its own only to add another namespace.

## Install

```sh
helm upgrade --install health-agent oci://ghcr.io/k8shealth/charts/k8shealth-agent-script \
  -n <namespace> --create-namespace --set namespace=<namespace>
```

## Related charts

| Chart | What it does |
|---|---|
| `health-manager` | The dashboard, API and incident engine |
| `k8shealth-policy` | Kyverno policy that injects the agent sidecar into labelled pods |

## License

Apache License 2.0.
