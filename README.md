# Observability Platform

A production-style **microservices platform written in Go**, instrumented end to end with **OpenTelemetry**, and shipped with a full observability stack: **Prometheus** (metrics), **Tempo** (distributed tracing), **Loki** (logs) and **Grafana** (dashboards).

The goal of the project is not only to *collect* telemetry, but to answer a concrete question:
**how fast can a failure be detected and diagnosed when metrics, traces and logs are correlated?**

The platform runs identically on **Docker Compose**, on a **local Kubernetes cluster**, and on **AWS EKS** via a **Helm chart**.

---

## Table of Contents

- [Architecture](#architecture)
- [Tech Stack](#tech-stack)
- [Features](#features)
- [Repository Layout](#repository-layout)
- [Quick Start (Docker Compose)](#quick-start-docker-compose)
- [Running on Kubernetes](#running-on-kubernetes)
- [Deploying to AWS EKS with Helm](#deploying-to-aws-eks-with-helm)
- [Telemetry Model](#telemetry-model)
- [Correlation: Metrics → Traces → Logs](#correlation-metrics--traces--logs)
- [Dashboards](#dashboards)
- [Fault Injection Experiments](#fault-injection-experiments)
- [Results](#results)
- [Configuration](#configuration)
- [Testing](#testing)
- [Roadmap](#roadmap)

---

## Architecture

```
                                 ┌──────────────┐
      client  ───────────────►   │ API Gateway  │  (Go, chi, OTel middleware)
                                 └──────┬───────┘
                                        │  HTTP + W3C traceparent
                        ┌───────────────┴───────────────┐
                        ▼                               ▼
                ┌───────────────┐               ┌───────────────┐
                │ Orders svc    │──────────────►│ Inventory svc │
                │ (Go)          │   gRPC/HTTP   │ (Go)          │
                └───────┬───────┘               └───────┬───────┘
                        │                               │
                        ▼                               ▼
                ┌───────────────┐               ┌───────────────┐
                │ PostgreSQL    │               │ PostgreSQL    │
                │ (orders)      │               │ (inventory)   │
                └───────────────┘               └───────────────┘

        every service exports OTLP  ─────────►  ┌──────────────────────┐
                                                │ OpenTelemetry        │
                                                │ Collector            │
                                                └───┬──────┬───────┬───┘
                                                    │      │       │
                                       metrics ◄────┘      │       └────► logs
                                            │              │                │
                                     ┌──────▼─────┐  ┌─────▼─────┐   ┌──────▼─────┐
                                     │ Prometheus │  │  Tempo    │   │   Loki     │
                                     └──────┬─────┘  └─────┬─────┘   └──────┬─────┘
                                            └──────────────┼────────────────┘
                                                           ▼
                                                     ┌───────────┐
                                                     │  Grafana  │
                                                     └───────────┘
```

Three Go services, one collector, three backends, one Grafana. Traces, metrics and logs all carry the same `trace_id`, so any point of any graph can be opened as a trace, and any trace span can be opened as the log lines it produced.

---

## Tech Stack

| Layer | Technology |
|---|---|
| Services | Go 1.22, `chi` router, `pgx` |
| Database | PostgreSQL 16 |
| Instrumentation | OpenTelemetry SDK for Go (traces, metrics, logs) |
| Pipeline | OpenTelemetry Collector (OTLP in, multi-exporter out) |
| Metrics | Prometheus (+ exemplars) |
| Tracing | Grafana Tempo |
| Logs | Grafana Loki (+ Promtail) |
| Visualisation | Grafana (provisioned dashboards + datasources) |
| Packaging | Docker, Docker Compose |
| Orchestration | Kubernetes, Helm |
| Cloud | AWS EKS |
| Load / chaos | `k6`, custom fault-injection middleware |

---

## Features

- **Three Go microservices** (API gateway, `orders`, `inventory`), each backed by PostgreSQL and each fully instrumented.
- **End-to-end distributed tracing.** Context propagates from the first HTTP hop through service-to-service calls down to individual SQL statements.
- **RED metrics** (Rate, Errors, Duration) per service and per endpoint, plus Go runtime and PostgreSQL pool metrics.
- **Structured JSON logs** enriched with `trace_id` / `span_id`, aggregated in Loki.
- **Exemplar-based correlation**: click a latency histogram bucket in Grafana → land on the exact slow trace → jump to that request's logs.
- **Fault injection** built into the services (latency, error rate, dependency failure) driven by config, so experiments are reproducible.
- **One chart, three environments.** The same Helm chart deploys to kind/minikube and to AWS EKS; only a values file changes.
- **Everything provisioned as code**: Grafana datasources, dashboards, alert rules and recording rules live in the repo.

---

## Repository Layout

```
.
├── cmd/
│   ├── gateway/            # API gateway
│   ├── orders/             # Orders service
│   └── inventory/          # Inventory service
├── internal/
│   ├── telemetry/          # OTel setup: tracer, meter, logger, propagators
│   ├── faults/             # Fault-injection middleware
│   ├── httpx/              # Shared HTTP server/client with instrumentation
│   └── store/              # PostgreSQL access layer (instrumented)
├── deploy/
│   ├── compose/            # docker-compose.yml + collector/prom/tempo/loki config
│   ├── k8s/                # Raw manifests (local cluster)
│   └── helm/
│       └── observability-platform/
│           ├── Chart.yaml
│           ├── values.yaml
│           ├── values-local.yaml
│           ├── values-eks.yaml
│           └── templates/
├── grafana/
│   ├── dashboards/         # JSON dashboards (provisioned)
│   └── provisioning/       # Datasources + dashboard providers
├── experiments/            # Fault-injection scenarios + k6 load scripts
├── docs/                   # Architecture notes, experiment write-ups
├── Makefile
└── README.md
```

---

## Quick Start (Docker Compose)

**Prerequisites:** Docker 24+, Docker Compose v2, Go 1.22 (only if you want to run services outside containers), `make`.

```bash
git clone https://github.com/Nidhalm1/Observability-Platform.git
cd Observability-Platform

# build the service images and start the whole stack
make up
```

That brings up: gateway, orders, inventory, two PostgreSQL instances, the OTel Collector, Prometheus, Tempo, Loki, Promtail and Grafana.

| Service | URL |
|---|---|
| API Gateway | http://localhost:8080 |
| Grafana | http://localhost:3000 (`admin` / `admin`) |
| Prometheus | http://localhost:9090 |
| Tempo | http://localhost:3200 |
| Loki | http://localhost:3100 |

Generate some traffic:

```bash
# a single order
curl -X POST http://localhost:8080/api/orders \
  -H 'Content-Type: application/json' \
  -d '{"sku":"SKU-1042","quantity":2}'

# sustained load
make load           # runs the k6 script in experiments/load/
```

Then open Grafana → **Platform Overview** and watch the request rate, error rate and p50/p95/p99 latency appear.

Tear down:

```bash
make down
```

---

## Running on Kubernetes

Tested on **kind** and **minikube**.

```bash
# 1. create a local cluster
kind create cluster --name obs --config deploy/k8s/kind-cluster.yaml

# 2. build and load the service images into the cluster
make images
make kind-load

# 3. install the chart with local values
helm install obs deploy/helm/observability-platform \
  --namespace observability --create-namespace \
  -f deploy/helm/observability-platform/values-local.yaml

# 4. check everything is up
kubectl -n observability get pods
```

Port-forward Grafana and the gateway:

```bash
kubectl -n observability port-forward svc/grafana 3000:3000
kubectl -n observability port-forward svc/gateway 8080:8080
```

The chart installs, in one release:

- the three Go services (Deployment + Service + HPA + PodDisruptionBudget),
- PostgreSQL as StatefulSets with PersistentVolumeClaims,
- the OpenTelemetry Collector as a Deployment (gateway mode) plus a DaemonSet for node-level logs,
- Prometheus, Tempo, Loki and Grafana as subchart dependencies,
- ConfigMaps for every pipeline config and every Grafana dashboard,
- ServiceMonitors so Prometheus discovers the services automatically.

---

## Deploying to AWS EKS with Helm

**Prerequisites:** `aws` CLI configured, `eksctl`, `kubectl`, `helm` 3.

```bash
# 1. create the cluster
eksctl create cluster -f deploy/eks/cluster.yaml

# 2. point kubectl at it
aws eks update-kubeconfig --region eu-west-3 --name observability-platform

# 3. push the images to ECR
make ecr-login
make images-push REGISTRY=<account>.dkr.ecr.eu-west-3.amazonaws.com

# 4. install
helm upgrade --install obs deploy/helm/observability-platform \
  --namespace observability --create-namespace \
  -f deploy/helm/observability-platform/values-eks.yaml \
  --set image.registry=<account>.dkr.ecr.eu-west-3.amazonaws.com \
  --set image.tag=$(git rev-parse --short HEAD)
```

What changes between local and EKS is only the values file:

| Concern | Local | EKS (`values-eks.yaml`) |
|---|---|---|
| Ingress | port-forward | AWS Load Balancer Controller (ALB) |
| Storage | local-path PVC | `gp3` StorageClass |
| Tempo / Loki backend | local filesystem | **S3** buckets |
| Credentials | static secrets | IRSA (IAM Roles for Service Accounts) |
| Scaling | 1 replica | HPA on CPU + request rate |
| TLS | none | ACM certificate on the ALB |

Uninstall:

```bash
helm uninstall obs -n observability
eksctl delete cluster -f deploy/eks/cluster.yaml
```

---

## Telemetry Model

**Traces.** Every inbound request creates a server span in the gateway. Context is propagated with the W3C `traceparent` header, so `gateway → orders → inventory → postgres` appears as a single trace. Database calls are wrapped by an instrumented `pgx` tracer, so each SQL statement gets its own span with the operation and table as attributes (no query values, so no PII in spans).

**Metrics.** Each service exposes:

| Metric | Type | Purpose |
|---|---|---|
| `http_server_requests_total` | counter | request rate & error rate (by route, status) |
| `http_server_duration_seconds` | histogram | latency p50/p95/p99, **with exemplars** |
| `http_client_duration_seconds` | histogram | latency of downstream calls |
| `db_query_duration_seconds` | histogram | PostgreSQL latency per operation |
| `db_pool_connections` | gauge | connection-pool saturation |
| `orders_created_total` | counter | business-level signal |
| `inventory_reservation_failures_total` | counter | business-level failure signal |

**Logs.** `slog` with a JSON handler. Every log record emitted inside a request carries `trace_id`, `span_id`, `service.name` and `deployment.environment`, which is exactly what makes the trace → logs jump work.

All three signals leave the services over **OTLP** to the Collector, which fans them out to Prometheus, Tempo and Loki. Nothing in the service code knows which backend is used, so swapping a backend is just a Collector config change.

---

## Correlation: Metrics → Traces → Logs

This is the part the project actually exists for.

1. **Metric → trace.** Latency histograms are exported with **exemplars** carrying the `trace_id` of a representative request. In Grafana, hovering the p99 line of a latency panel shows a green diamond; clicking it opens that exact trace in Tempo. You go from *"p99 spiked at 14:32"* to *the individual slow request* in one click.

2. **Trace → logs.** Tempo's datasource is configured with a **trace-to-logs** link against Loki, using the query `{service_name="$${__span.tags["service.name"]}"} | json | trace_id="$${__trace.traceId}"`. From any span, you get the log lines that request produced.

3. **Logs → trace.** Loki's datasource declares a **derived field** on `trace_id` that links straight back to Tempo. Starting from an error log, you recover the whole request path.

4. **Trace → metrics.** Tempo's **span metrics** processor generates RED metrics per span, so a service that only appears deep in a trace still shows up on the service graph.

The practical effect: **no context switching and no manual grep.** A single alert leads to a trace leads to a log line, and the diagnosis loop stays inside Grafana.

---

## Dashboards

All dashboards are provisioned from `grafana/dashboards/`, so no manual clicking is required.

| Dashboard | Contents |
|---|---|
| **Platform Overview** | Global RED metrics, error budget, service map, active alerts |
| **Service Detail** | Per-service and per-endpoint rate/errors/latency, with exemplars |
| **Database** | Query latency by operation, pool saturation, slow-query trace links |
| **Traces Explorer** | Tempo search, service graph, span-metrics view |
| **Logs** | Loki live tail filtered by service, level and `trace_id` |
| **Experiments (MTTD/MTTR)** | Annotated timeline of each fault-injection run with detection and diagnosis timestamps |

Alerting rules cover: error rate above 5% for 2 minutes, p99 latency above 1s for 5 minutes, database pool saturation, and absence of telemetry from any service.

---

## Fault Injection Experiments

The services embed a fault-injection middleware (`internal/faults`) that can be switched on at runtime, per service and per route:

| Fault | What it does |
|---|---|
| `latency` | Adds a configurable delay (fixed or distributed) before handling |
| `error` | Returns 5xx for a percentage of requests |
| `dependency` | Makes a downstream call fail or hang |
| `db_slow` | Injects delay into the PostgreSQL layer |
| `saturation` | Caps the connection pool to force queuing |

A scenario is a YAML file in `experiments/`:

```yaml
name: inventory-latency-p95
target: inventory
fault:
  type: latency
  delay: 800ms
  ratio: 0.30
load:
  script: experiments/load/orders.js
  vus: 50
  duration: 5m
```

Run one:

```bash
make experiment SCENARIO=experiments/inventory-latency-p95.yaml
```

The runner injects the fault, drives load with k6, writes Grafana annotations at fault start/stop, and records:

- **MTTD**: time between fault start and the alert firing.
- **MTTD (visual)**: time until the anomaly is visible on the overview dashboard.
- **MTTR diagnosis**: number of clicks and elapsed time to identify the *responsible service and operation* starting from the alert.


---

## Configuration

Every service is configured by environment variable:

| Variable | Default | Description |
|---|---|---|
| `SERVICE_NAME` | *(required)* | Sets `service.name` on all telemetry |
| `HTTP_ADDR` | `:8080` | Listen address |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://otel-collector:4317` | Collector endpoint |
| `OTEL_TRACES_SAMPLER` | `parentbased_traceidratio` | Sampling strategy |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | Sample ratio (lower it in production) |
| `DEPLOYMENT_ENV` | `local` | `local` / `k8s` / `eks` |
| `POSTGRES_DSN` | *(required)* | PostgreSQL connection string |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `FAULTS_ENABLED` | `false` | Enables the fault-injection middleware |
| `FAULTS_CONFIG` | *(none)* | Path to the active scenario file |

In Kubernetes these come from the chart's `values.yaml`; secrets (database credentials, S3 access) come from Kubernetes Secrets, and on EKS from IRSA rather than static keys.

---

## Testing

```bash
make test          # unit tests
make test-integration   # spins up PostgreSQL + collector via testcontainers
make lint          # golangci-lint
make helm-lint     # helm lint + helm template validation
```

Integration tests assert on the telemetry itself, not just the business logic: a test asserts that a request produces a trace of the expected shape, that the histogram gets an exemplar attached, and that the emitted log line carries the same `trace_id` as the span.

---

## Roadmap

- [ ] Pyroscope for continuous profiling, correlated with traces
- [ ] SLO definitions with multi-window burn-rate alerts
- [ ] OpenTelemetry Operator + auto-instrumentation injection
- [ ] Cost analysis: sampling strategies vs. diagnosis quality
- [ ] GitOps deployment with Argo CD

---

## License

MIT

## Author

**Nidhal Moussa** &middot; [GitHub](https://github.com/Nidhalm1)
