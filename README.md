# ingress-host-prefix-webhook

A Kubernetes mutating admission webhook that adds a prefixed duplicate host
rule to every Ingress. On every Ingress `CREATE`/`UPDATE` (outside
`kube-system` and the webhook's own namespace), for each `spec.rules` entry
with a non-empty `host` it adds one extra rule with the same host prefixed
by a configurable string (default `prefixed-`), copying that rule's `http`
paths/backend. If `spec.tls` already lists the original host, the prefixed
host is mirrored into that TLS entry's `hosts` too (list only — no
certificate actually covers the new hostname, and none is implied). Both
mutations are idempotent: repeated `UPDATE`s on an already-mutated Ingress
are a no-op, they don't keep stacking duplicates.

```
myapp.example.com  ->  myapp.example.com, prefixed-myapp.example.com
```

## Repo layout

```
webhook/     Go source for the webhook server (admission handler + /metrics)
chart/       Helm chart that installs everything
test/        A sample Ingress used to manually verify the mutation
monitoring/  Standalone manifests wiring an external Grafana (via
             grafana-operator) to the metrics this chart exposes — not
             part of the Helm chart itself, see "Metrics & dashboard" below
HANDOFF.md   Full build log: every issue hit during development, root
             causes, and exact fixes — read this if something here doesn't
             behave as documented
```

## Requirements

- A Kubernetes cluster (developed and tested against local k3s v1.25.16 via
  Rancher Desktop). Only `admissionregistration.k8s.io/v1` is used — no
  CEL/`matchConditions` (those need 1.27+).
- Helm 3.
- A container image for `webhook/` reachable by the cluster's nodes. This
  chart was developed with a **local-only** image (`pullPolicy: Never`,
  never pushed to any registry) — see "Building the image" below, especially
  if you're also on Rancher Desktop.
- Optional, only if you enable the matching chart feature: Cilium (for
  `ciliumNetworkPolicy`), kube-prometheus-stack + grafana-operator (for
  `metrics`/dashboards), KEDA (for `keda` autoscaling).

## Install

```bash
helm install ingress-host-prefix-webhook ./chart \
  --create-namespace \
  -n ingress-host-prefix-webhook-system
```

No `--set` flags are required for a basic install — see `chart/values.yaml`
for every default. To upgrade in place after a values or image change:

```bash
helm upgrade ingress-host-prefix-webhook ./chart -n ingress-host-prefix-webhook-system
```

The chart generates its own self-signed CA/serving cert at install time
(Helm's `genCA`/`genSignedCert`) and reuses it on upgrade via `lookup`
instead of regenerating — no cert-manager dependency.

## Verify

```bash
kubectl -n ingress-host-prefix-webhook-system get pods
kubectl apply -f test/test-ingress.yaml
kubectl get ingress test-ingress -n default -o jsonpath='{.spec.rules[*].host}{"\n"}'
```

Expect `myapp.example.com second.example.com prefixed-myapp.example.com prefixed-second.example.com`.
Re-applying the same file (or any no-op `UPDATE`) should leave the host
count unchanged — that's the idempotency guarantee.

## Configuration (`values.yaml`)

| Key | Default | What it does |
|---|---|---|
| `hostPrefix` | `prefixed-` | String prepended to each host to build the duplicate rule |
| `failurePolicy` | `Ignore` | `Ignore` lets Ingress writes through unmutated if the webhook is down; flip to `Fail` once trusted |
| `image.repository`/`tag`/`pullPolicy` | local `ingress-host-prefix-webhook:dev`, `Never` | See "Building the image" |
| `resources` | 10m/32Mi requests, 100m/64Mi limits | Also required for the KEDA CPU trigger to function |
| `networkPolicy.enabled` | `true` | Standard `NetworkPolicy`: only the webhook port in, no egress |
| `ciliumNetworkPolicy.enabled` | `false` | Cilium-native equivalent. **Opt-in** — the CRD not existing fails the whole release on a non-Cilium cluster |
| `metrics.enabled` | `true` | `/metrics` endpoint + `ServiceMonitor` + Prometheus-scrape NetworkPolicy rule |
| `keda.enabled` | `false` | `ScaledObject` autoscaling. **Opt-in**, same CRD-safety reasoning as Cilium |

## Building the image

The image is never pushed to a registry — `pullPolicy: Never` means it must
already exist wherever the pod is scheduled:

```bash
docker build --platform linux/amd64 -t ingress-host-prefix-webhook:dev ./webhook
```

**If you're on Rancher Desktop on Windows**, be aware the plain Windows
`docker` CLI and the k3s node's actual container runtime can be two
completely separate engines (Docker Desktop's dockerd vs. a dockerd inside
a dedicated `rancher-desktop` WSL2 distro) — a build that only lands in the
former will leave the node stuck in `ErrImageNeverPull` even though
`docker images` shows the tag locally. The fix used throughout this
project's development:

```bash
docker build --platform linux/amd64 -t ingress-host-prefix-webhook:dev ./webhook
docker save ingress-host-prefix-webhook:dev -o webhook-dev-image.tar
wsl -d rancher-desktop -- docker load -i /mnt/<path-to>/webhook-dev-image.tar
```

Then force the running pod to pick up the new bytes (same tag, so kubelet
won't do this on its own):

```bash
kubectl -n ingress-host-prefix-webhook-system rollout restart deployment/ingress-host-prefix-webhook
```

See `HANDOFF.md` for the full root-cause writeup if this doesn't match your
setup.

## Metrics & dashboard

With `metrics.enabled` (default on), the webhook exposes Prometheus metrics
on a separate plain-HTTP port (default `8080`, kept apart from the TLS
admission port so Prometheus never needs to trust the self-signed admission
cert):

- `webhook_admission_requests_total{outcome}` — counter, `outcome` ∈
  `mutated`/`unmutated`/`error`
- `webhook_build_info{version}` — gauge fixed at `1`, the `version` label
  (from the `APP_VERSION` env var) is the useful part
- `process_cpu_seconds_total`, `process_resident_memory_bytes`,
  `go_memstats_*`, etc. — free via the default Prometheus Go client registry

A `ServiceMonitor` is created so any Prometheus Operator install with
permissive `serviceMonitorSelector` will pick it up automatically. The
`monitoring/` directory holds everything needed to wire this into a full
stack that was **not** installed by default (deliberately outside the
Helm chart, since it's shared cluster-wide infrastructure, not scoped to one
release):

```bash
# 1. Prometheus + Grafana
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
  -n monitoring --create-namespace -f monitoring/kube-prometheus-stack-values.yaml

# 2. grafana-operator — pin the version; recent releases ship CRDs using
#    CEL "format" validators unsupported on Kubernetes 1.25 (see HANDOFF.md)
helm repo add grafana https://grafana.github.io/helm-charts
helm install grafana-operator grafana/grafana-operator --version 5.15.1 -n monitoring

# 3. Point grafana-operator at that Grafana instance + push the dashboard
kubectl apply -f monitoring/grafana-instance.yaml -f monitoring/grafana-dashboard.yaml
```

The dashboard (`monitoring/grafana-dashboard.yaml`) has panels for memory,
CPU, admission request rate by outcome, and the running version. It assumes
the webhook chart is installed under release name
`ingress-host-prefix-webhook` (adjust the panel queries' `job` label if you
use a different release name) and that kube-prometheus-stack is in
namespace `monitoring` under release name `kube-prometheus-stack`.

## Autoscaling (KEDA)

With `keda.enabled=true` (requires KEDA already installed on the cluster),
the chart adds a `ScaledObject` with two triggers — either one alone can
scale up:

- **CPU** — 70% utilization (needs `resources.requests.cpu`, already set)
- **Prometheus** — `sum(rate(webhook_admission_requests_total[1m])) > 5`,
  queried against kube-prometheus-stack's Prometheus

```bash
helm upgrade ingress-host-prefix-webhook ./chart -n ingress-host-prefix-webhook-system \
  --set keda.enabled=true
kubectl -n ingress-host-prefix-webhook-system get scaledobject,hpa
```

The CPU trigger needs a working `metrics-server` in the cluster; the
Prometheus trigger works independently of that and only needs the metrics
stack above.

## Known limitations

- `ciliumNetworkPolicy` was only template-validated (`helm lint`/`template`,
  `kubectl apply --dry-run=client`) — there's no Cilium cluster available in
  this project's environment to confirm live enforcement.
- The Grafana dashboard's datasource is referenced by the plain name
  `"Prometheus"`, relying on kube-prometheus-stack's Grafana self-provisioning
  a datasource under that exact name — check this first if panels show no
  data on a different setup.
- This cluster's `metrics-server` was found to be non-functional (pre-existing,
  unrelated to this project) during KEDA validation, which starves the CPU
  autoscaling trigger of data; the Prometheus trigger is unaffected.

See `HANDOFF.md` for the complete history of every issue found during
development/validation and exactly how each was fixed.
