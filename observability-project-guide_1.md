# Building an Observability Platform That Actually Impresses Datadog

**A concept-first, step-by-step guide for someone who has never instrumented a distributed system.**

---

## 0. Read this before anything else

### 0.1 The mistake to avoid

This guide is built as the answer to a code review — not of your work, but of a widely-copied "cloud-native observability" portfolio repo you asked me to look at. It's the template most people follow for this kind of project, and a reviewer took it apart. Their criticism compresses to one sentence:

> You installed observability tools. You did not build an observable system.

Installing Prometheus with a Helm chart takes 10 minutes and demonstrates nothing. What a Datadog interviewer wants to see is the opposite skill: **you took a system, decided what to measure, measured it correctly, broke it on purpose, and used your own telemetry to find the cause.**

Everything in this guide is organised around that one idea.

### 0.2 The spec you were given is too big — cut it

The proposal in your document lists Redis, memory leaks, CPU burners, ArgoCD, EKS, tail sampling, Trivy scanning, and five dashboards. If you try to do all of it as a beginner you will spend three weeks on YAML and produce another shallow repo.

**Cut ruthlessly. Depth beats breadth in an interview.** Here is what stays and what goes:

| Keep | Drop (or make it a stretch goal) |
|---|---|
| 3 Go services, real HTTP calls between them | Redis (adds a component, teaches nothing new) |
| PostgreSQL with a real schema and real queries | Memory leak / CPU burner (fake, not diagnosable) |
| RED metrics with histograms | ArgoCD |
| Distributed tracing with real propagation | Tail sampling (do it only if you finish early) |
| Logs correlated to traces | 5 dashboards (2 good ones beat 5 empty ones) |
| **Realistic** faults + written investigations | Datadog Agent swap (great stretch goal, not core) |
| **EKS, Trivy, Docker Hub, GitHub OIDC** (Phases 11–13) | — |

The cloud/CI layer stays, but it comes **last** and it runs for four days, not four weeks. Section 0.5 explains why the ordering matters more than the inclusion.

One service, fully and correctly instrumented, with one written root-cause investigation, is worth more than a nine-component cluster nobody can explain.

### 0.3 Time budget

Roughly 3 weeks at ~2 hours/day. Phases 1–7 are the project. Phases 8–10 are polish. If you run out of time, **ship after Phase 7 with a good README.** An unfinished Kubernetes migration is invisible to a reviewer; a missing investigation write-up is fatal.

### 0.4 Prerequisites

You need to be comfortable writing a Go HTTP handler and running `docker compose up`. You do not need to know Prometheus, OpenTelemetry, or Kubernetes — that is what you are here to learn.

**This guide assumes you have built nothing yet.** Phase 1 is three plain Go services with zero instrumentation. Nothing here builds on top of an existing observability project, and no phase expects you to already have metrics, traces, or a cluster. Start at Phase 0 with an empty directory.

### 0.5 Why the cloud layer comes last

EKS, Trivy, Docker Hub and OIDC are all in this guide. But the order is deliberate, for two reasons.

**Money.** An EKS cluster bills from the moment it exists — the control plane alone is $0.10/hour whether or not a pod runs. You will redeploy fifty times while debugging your instrumentation. Do those fifty redeploys on `kind` (free) and the last four on EKS (~$17 of free credits). Same screenshots, 95% less cost.

**Signal.** Provisioning a cluster and installing Helm charts is the part of that other repo the reviewer dismissed. It is not worthless — Datadog runs enormous Kubernetes fleets and you should be able to talk about it — but it is table stakes, not the differentiator. If you spend week one on `CrashLoopBackOff` you will arrive at week three with a beautiful cluster running an uninstrumented app, which is exactly the failure you are trying to avoid.

So: Phases 1–10 on your laptop, Phases 11–13 on AWS, in that order, no exceptions.

---

# PART ONE — THE CONCEPTS

Do not skip this. If you copy code without understanding these ideas, the interview deep-dive will expose it in about four minutes.

## 1. Monitoring vs observability

**Monitoring** answers questions you decided to ask in advance. "Is CPU above 80%?" You wrote that check because you already knew CPU could be a problem.

**Observability** is the property of a system that lets you answer questions you *didn't* anticipate. "Why is p99 latency 4 seconds, but only for French users, and only on the checkout endpoint, and only since Tuesday?" Nobody wrote a check for that. You need enough signal in the system to reconstruct the answer after the fact.

The practical difference: monitoring produces alerts, observability produces *explanations*. Interviewers at an observability company care about the second one.

## 2. The three signals

### 2.1 Metrics — cheap, aggregate, low detail

A metric is a number over time. `http_requests_total = 1,482,301`. Storage cost is roughly constant no matter how much traffic you have, because you're storing a counter, not events.

**What they're good at:** telling you *that* something is wrong, fast, across the whole system.
**What they're bad at:** telling you *which* request was wrong. You cannot ask a metric "show me the slow one."

The four Prometheus metric types:

- **Counter** — only goes up (or resets to zero on restart). Requests served, errors, bytes sent. You almost never read a counter directly; you read its `rate()`.
- **Gauge** — goes up and down. Current memory, open DB connections, queue depth.
- **Histogram** — buckets of observations. This is the one that matters for latency, and the one that reviewed repo was missing.
- **Summary** — client-side quantiles. Avoid it; you can't aggregate summaries across instances.

### 2.2 Why histograms, and why averages are a lie

Suppose 99 requests take 10 ms and one takes 5 seconds. The average is 60 ms — which looks fine and describes literally none of your requests. The user who waited 5 seconds is the one who churns.

A histogram solves this by counting requests into latency buckets:

```
http_request_duration_seconds_bucket{le="0.005"}  842
http_request_duration_seconds_bucket{le="0.01"}   1201
http_request_duration_seconds_bucket{le="0.025"}  1290
http_request_duration_seconds_bucket{le="0.1"}    1301
http_request_duration_seconds_bucket{le="+Inf"}   1302
```

These are *cumulative* buckets: `le="0.01"` means "requests that took 10 ms **or less**." From this you can estimate any percentile after the fact:

```promql
histogram_quantile(
  0.99,
  sum by (le, route) (rate(http_request_duration_seconds_bucket[5m]))
)
```

**The single most important rule about percentiles:** you cannot average them. `avg(p99_of_pod_a, p99_of_pod_b)` is not the p99 of the system — it's a meaningless number. You must aggregate the *buckets* (`sum by (le)`) and compute the quantile from the sum. This is a classic interview question. Know it.

The trade-off, which you should also know: `histogram_quantile` is an interpolation within a bucket, so its accuracy depends entirely on where you put your bucket boundaries. If your highest finite bucket is 1 second and your real p99 is 4 seconds, Prometheus will report something close to 1 second and you will be confidently wrong.

### 2.3 Cardinality — the concept that will get you hired or rejected

Prometheus stores one time series per unique combination of metric name and label values. `http_requests_total{route="/orders", method="POST", status="200"}` is one series.

If you add `user_id` as a label and you have 100,000 users, you just created 100,000 series **per route per method per status**. Memory explodes, queries time out, the database falls over. This is called a cardinality explosion, and it is the #1 way people break their own monitoring.

**Rules:** labels must be low-cardinality and bounded. `route`, `method`, `status_code`, `service` — fine. `user_id`, `order_id`, `trace_id`, `email`, raw URL paths with IDs in them — never.

And note the corollary, which is the whole reason traces exist: high-cardinality data (which user, which order, which request) has to go somewhere else. That somewhere is traces and logs.

This matters extra for Datadog specifically: their custom-metrics pricing is based on the number of unique metric-tag combinations. Cardinality control is not academic there — it's the business model. If you can talk about it fluently you will stand out.

### 2.4 The RED method

For any request-driven service, measure three things:

- **R**ate — requests per second
- **E**rrors — failed requests per second
- **D**uration — latency distribution

The reviewed repo had Rate only, which is why it got flagged. RED gives you a complete first-order view of service health, and it's the template for every dashboard you'll build.

Its sibling is **USE** (Utilization, Saturation, Errors), which applies to *resources* rather than services — CPU, disk, connection pools. Use RED for services, USE for the things underneath them. Knowing when to apply which is a nice thing to say out loud in an interview.

### 2.5 Traces — expensive, detailed, per-request

A **span** is one unit of work: an HTTP handler, a DB query, an outbound call. It has a name, a start and end time, attributes, and a status.

A **trace** is a tree of spans that share a trace ID and represent one request as it moves through your system:

```
trace_id=abc123
└─ POST /orders                (gateway)      412ms
   └─ POST /orders             (orders)       405ms
      ├─ SELECT products       (postgres)      12ms
      └─ GET /inventory/check  (inventory)    380ms   ← here's your problem
         └─ SELECT stock       (postgres)     375ms   ← no, here it is
```

Unlike metrics, traces carry high-cardinality data happily. `user_id`, `order_id`, the actual SQL — all fine as span attributes.

**Context propagation** is the mechanism that makes this work, and the thing the reviewed repo didn't have. When the gateway calls the orders service, it must send an HTTP header:

```
traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
             ^  ^                                ^                ^
             |  trace-id (16 bytes)              parent span-id   flags (01 = sampled)
             version
```

The orders service reads that header, and its own span becomes a *child* of the gateway's span. Without propagation you get N disconnected single-span traces instead of one tree — which is exactly what the reviewer described. This is the W3C Trace Context standard, and OpenTelemetry implements it for you; your job is to make sure you use an instrumented HTTP client and server so the header is actually written and read.

**Sampling** exists because storing every span at scale is prohibitive. *Head sampling* decides at the start of the trace (keep 1%, cheap, but you lose most of the interesting failures). *Tail sampling* decides after the trace completes (keep everything slow or errored, much more useful, but the collector must buffer whole traces in memory). For this project use 100% sampling — your traffic is tiny — but be ready to explain the trade-off.

### 2.6 Logs — high detail, high cost, hard to aggregate

A log line is an event with text. Two things separate a professional from a beginner here:

**Structured logging.** Log JSON, not sentences. `log.Printf("order %d failed for user %s", id, user)` cannot be queried. This can:

```json
{"time":"2026-08-03T10:12:44Z","level":"ERROR","service":"orders",
 "msg":"inventory check failed","order_id":8812,"status":503,
 "trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}
```

**Trace correlation.** That `trace_id` field is the entire point. It is what lets you go from "this request was slow" to "show me every log line that request produced, across all three services." Without it your logs and traces are two separate piles of data and you're back to grepping.

### 2.7 The correlation workflow — the thing you're actually building

Each signal alone is weak. The value is in moving between them:

```
Alert / dashboard        "p99 on /orders jumped from 80ms to 3.4s at 14:02"   ← METRICS: what & when
        │  (click an exemplar)
        ▼
Trace                    "380ms of the 412ms is inside inventory's DB span"   ← TRACES: where
        │  (click trace_id → logs)
        ▼
Logs                     "connection pool wait timeout, pool size 5"          ← LOGS: why
```

Metrics tell you **what** and **when**. Traces tell you **where**. Logs tell you **why**. If your README shows one screenshot of each step of that chain for a real failure you diagnosed, you have already beaten 90% of portfolio projects.

The mechanism that connects step 1 to step 2 is called an **exemplar**: a trace ID attached to a specific histogram observation, so Grafana can render clickable dots on your latency graph that jump straight to a slow trace. Most people have never implemented this. Do it.

## 3. The tools, and what each one is actually for

Beginners confuse these constantly. Learn the division of labour:

| Tool | Role | Think of it as |
|---|---|---|
| **OpenTelemetry SDK** | Library *inside your Go code* that creates spans, metrics, logs | The instrumentation |
| **OTel Collector** | Separate process that receives telemetry, processes it, routes it | The pipeline / router |
| **Prometheus** | Time-series database for metrics. **Pulls** by scraping HTTP endpoints | The metrics store |
| **Tempo** | Trace store. Cheap because it only indexes trace ID | The trace store |
| **Loki** | Log store. Indexes labels, not log content | The log store |
| **Grafana** | Query UI across all three | The face |

Two points worth understanding rather than memorising:

**Push vs pull.** Prometheus *pulls* — it scrapes `/metrics` on a schedule. This means Prometheus needs to discover and reach your services, and a service that's down simply stops being scraped (which is itself a signal: `up == 0`). Datadog's Agent, by contrast, *pushes*. Neither is universally right; pull is easier to reason about in Kubernetes, push works better across network boundaries and for short-lived jobs.

**Why the Collector.** You could export straight from your app to Tempo and Prometheus. The Collector exists so your application code doesn't know or care where telemetry goes — you can add batching, retries, redaction of PII, sampling, or switch backends entirely by editing one YAML file instead of redeploying every service. When you later swap Tempo for Datadog APM as a stretch goal, that's a Collector config change and nothing else. Say that in the interview.

## 4. How this maps to Datadog

You're building the open-source stack, but the concepts transfer one-to-one, and being able to draw the mapping shows you understand principles rather than products:

| You build | Datadog equivalent |
|---|---|
| OTel SDK in Go | `dd-trace-go`, or OTel via the Datadog exporter |
| OTel Collector | Datadog Agent (also embeds an OTLP receiver) |
| Prometheus + PromQL | Datadog Metrics + custom metrics (priced on tag cardinality) |
| Tempo | Datadog APM |
| Loki | Datadog Log Management |
| Grafana | Datadog Dashboards |
| `histogram_quantile` | Distribution metrics |
| Exemplars | Trace-to-metric correlation in APM |

Datadog is also a major contributor to OpenTelemetry, and OTLP ingestion is a first-class path into their platform. Learning OTel properly is not a detour from Datadog — it's directly on the path.

---

# PART TWO — WHAT YOU'RE BUILDING

## 5. Architecture

```
        k6 (load generator)
              │
              ▼
     ┌─────────────────┐
     │  API Gateway    │  validates, routes, sets timeouts
     └────────┬────────┘
              │ HTTP + traceparent
              ▼
     ┌─────────────────┐        ┌──────────────────┐
     │ Orders Service  │───────▶│ Inventory Service│
     └────────┬────────┘  HTTP  └────────┬─────────┘
              │                          │
              └──────────┬───────────────┘
                         ▼
                    PostgreSQL
```

All three services export OTLP to a Collector, which fans out to Prometheus, Tempo, and Loki, all read by Grafana.

## 6. Why exactly this shape

Every component earns its place:

- **Three services, not one** — you cannot demonstrate distributed tracing with one service. Two is the minimum; three lets you build a two-level trace tree where the root cause is *not* the immediate child, which is a far more realistic debugging story.
- **A real database** — the majority of production latency problems are database problems. A `time.Sleep` is not a bug you can diagnose; a missing index is.
- **Gateway calls Orders calls Inventory calls Postgres** — this is the shape that lets you build the killer investigation: the gateway is slow, Orders looks slow, but the actual fault is two hops down. That story is the whole project.

## 7. Business logic (keep it boring)

Deliberately trivial, so all the interest is in the telemetry:

- `POST /orders` — create an order. Gateway validates → Orders inserts a row → calls Inventory to check stock → returns.
- `GET /orders/{id}` — read an order with its line items.
- `GET /inventory/{sku}` — Inventory reads a stock row.

Three tables: `orders`, `order_items`, `inventory`. Seed `inventory` with ~200,000 rows — you need enough data that a missing index actually hurts. This matters: with 100 rows, Postgres scans the whole table in microseconds and your "missing index" bug is undetectable.

---

# PART THREE — IMPLEMENTATION, STEP BY STEP

Each phase has a goal, the work, and a **"done when"** check. Do not move on until the check passes. Commit at the end of each phase — your git history becomes evidence of how you work.

## Phase 0 — Environment (½ day)

Install: Go 1.22+, Docker + Docker Compose, `psql` client, `k6`, and `curl`. Kubernetes tools (`kind`, `kubectl`, `helm`) come later — you don't need them yet.

Create the repo:

```
obs-platform/
├── services/{gateway,orders,inventory}/
├── deploy/compose/
├── db/migrations/
├── load/
└── docs/investigations/
```

**Done when:** `go version`, `docker compose version`, and `k6 version` all work.

> **Start with Docker Compose, not Kubernetes.** Kubernetes adds networking, DNS, service discovery, and YAML debugging on top of a problem you don't understand yet. Every hour you spend on `CrashLoopBackOff` in week one is an hour not spent learning observability. Migrate in Phase 11, once everything works.

## Phase 1 — Three plain services, zero observability (2 days)

Write three Go HTTP services with no instrumentation at all. Real handlers, real `database/sql` + `pgx`, real HTTP calls between services. Compose file with the three services plus Postgres. Migrations that create the schema and seed 200k inventory rows.

Two things to get right now, because retrofitting them is painful:

**Pass `context.Context` everywhere.** Every function that does I/O takes `ctx` as its first parameter, and every DB call uses `db.QueryContext(ctx, ...)` not `db.Query(...)`. This is how trace context travels through your program. If you skip it, Phase 4 becomes a rewrite.

**Set explicit timeouts on every outbound call.** `http.Client{Timeout: 2*time.Second}`, and `context.WithTimeout` around DB calls. A Go HTTP client with no timeout waits forever by default — that's how a slow dependency turns into a full outage upstream. You'll demonstrate this deliberately in Phase 7.

**Done when:** `curl -X POST localhost:8080/orders -d '{"sku":"ABC","qty":2}'` returns 201, and you can see the row in `psql`.

## Phase 2 — Metrics, written by hand (2 days)

Use `prometheus/client_golang` directly at first, *not* the OTel metrics SDK. You'll understand the data model far better if you declare the metrics yourself.

Write one middleware, shared by all three services:

```go
var (
    httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "http_requests_total",
        Help: "Total HTTP requests.",
    }, []string{"service", "method", "route", "status"})

    httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "http_request_duration_seconds",
        Help:    "HTTP request latency.",
        Buckets: []float64{.005, .01, .025, .05, .1, .2, .3, .5, 1, 2.5, 5, 10},
    }, []string{"service", "method", "route"})
)

func Metrics(service string, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        rec := &statusRecorder{ResponseWriter: w, status: 200}
        next.ServeHTTP(rec, r)

        route := chi.RouteContext(r.Context()).RoutePattern() // "/orders/{id}", NOT "/orders/8812"
        httpRequests.WithLabelValues(service, r.Method, route,
            strconv.Itoa(rec.status)).Inc()
        httpDuration.WithLabelValues(service, r.Method, route).
            Observe(time.Since(start).Seconds())
    })
}
```

Four details that are the whole lesson:

1. **`route`, not `r.URL.Path`.** Using the raw path means `/orders/1`, `/orders/2`, `/orders/3` each become a separate time series. That's the cardinality explosion from §2.3, and it's the most common real-world instance of it. Use your router's route *pattern*.
2. **Bucket boundaries are a design decision.** They must span your actual latency range. If everything lands in the `+Inf` bucket, your percentiles are garbage.
3. **Put your future SLO threshold on a bucket boundary.** Note the `.2` and `.3` in the list above — they aren't in the standard default set. If you later declare an SLO like "99% of `/orders` under 300 ms" (Phase 14), the correct way to measure it is not `histogram_quantile`; it's a ratio of counts: `rate(..._bucket{le="0.3"}[5m]) / rate(..._count[5m])`. That query only works if `0.3` is an actual bucket edge. If it isn't, Prometheus interpolates — and an interpolated SLO is a guess, not a measurement. Decide your candidate thresholds now, because **changing buckets later invalidates your history**: the old series has different boundaries, so you lose the comparison across the change.
4. **`status` on the counter but not the histogram.** Errors are usually fast (a 400 returns immediately), so mixing them into your latency distribution skews it. Rate and Errors come from the counter; Duration comes from the histogram. Trade-off to be aware of: this means you can't filter the latency SLI to successful requests only. For this project that's fine — say so in the README rather than discovering it in the interview.

**One Prometheus setting to get right now:** default retention is 15 days. A 30-day error budget over 15 days of data silently computes over whatever exists, and looks fine. Set `--storage.tsdb.retention.time=45d` in your Compose file from day one.

Add Prometheus to Compose, scrape all three services, then live in the Prometheus query UI for an hour and write these yourself:

```promql
# Rate
sum by (service) (rate(http_requests_total[5m]))

# Error ratio
sum by (service) (rate(http_requests_total{status=~"5.."}[5m]))
  / sum by (service) (rate(http_requests_total[5m]))

# Duration p99
histogram_quantile(0.99,
  sum by (le, service, route) (rate(http_request_duration_seconds_bucket[5m])))
```

**Done when:** you can explain out loud why `rate()` is inside `sum()` and not outside. (Because `rate()` needs the raw per-series counter to detect resets; summing counters across restarting pods first gives you a nonsense sawtooth.)

## Phase 3 — Structured logging (½ day)

Go 1.21+ ships `log/slog`. Configure a JSON handler, log to stdout, and never use `fmt.Printf` again. Include `service` on every line. You'll add `trace_id` in Phase 6.

**Done when:** `docker compose logs orders | jq .` parses cleanly.

## Phase 4 — Tracing in one service (1 day)

Set up the OTel SDK in the orders service: a `TracerProvider` with an OTLP exporter, a batch span processor, and a `Resource` carrying `service.name`. Wrap your handler with `otelhttp.NewHandler`. Instrument the database with `github.com/XSAM/otelsql` so queries become spans automatically.

Add the OTel Collector and Tempo to Compose, wire Tempo as a Grafana data source, and look at your first trace.

**Done when:** one request produces a trace with a parent HTTP span and a child SQL span.

## Phase 5 — Real distributed tracing (1 day)

This is the phase that addresses the exact flaw the reviewer found in that other repo.

For propagation to work, both ends must cooperate:

```go
// Global propagator — do this once at startup, in EVERY service.
otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{}, propagation.Baggage{},
))

// Outbound: the transport injects `traceparent` into the request headers.
client := &http.Client{
    Transport: otelhttp.NewTransport(http.DefaultTransport),
    Timeout:   2 * time.Second,
}

// You MUST use the request-scoped context, or the header carries nothing.
req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)  // ctx from r.Context()
resp, err := client.Do(req)
```

The classic beginner bug: calling `http.NewRequest` (no context) or `context.Background()`. The code compiles, the request works, spans are created — and every trace is a single orphaned span, because the context carrying the trace ID never reached the injector. That's precisely the symptom the reviewer described in the repo that prompted this guide.

**Done when:** in Tempo you see one trace, one trace ID, spans from all three services, and a genuine parent-child tree. Screenshot it — this goes in the README.

## Phase 6 — Correlation (1 day)

Three connections to build.

**Logs → traces.** Pull the trace ID out of the context and attach it to every log line:

```go
func LogWith(ctx context.Context) *slog.Logger {
    sc := trace.SpanContextFromContext(ctx)
    if !sc.IsValid() {
        return slog.Default()
    }
    return slog.With("trace_id", sc.TraceID().String(),
                     "span_id", sc.SpanID().String())
}
```

Then in Grafana, add a **derived field** on the Loki data source: regex the `trace_id` out and link it to Tempo. Now every log line has a clickable button that opens its trace.

**Traces → logs.** Configure Tempo's "trace to logs" so a span links back to the Loki query for that trace ID. The loop closes in both directions.

**Metrics → traces (exemplars).** Run Prometheus with `--enable-feature=exemplar-storage`, and record an exemplar with each histogram observation:

```go
if o, ok := httpDuration.WithLabelValues(...).(prometheus.ExemplarObserver); ok {
    o.ObserveWithExemplar(time.Since(start).Seconds(),
        prometheus.Labels{"trace_id": traceID})
}
```

Note the asymmetry, and understand why it exists: the trace ID is *forbidden* as a metric label (unbounded cardinality) but *allowed* as an exemplar, because exemplars are sampled side-data attached to a bucket rather than a dimension of the series. Explaining that distinction is a strong signal in an interview.

**Done when:** you can start on a latency graph, click a dot, land on a trace, click a span, and read the logs for that exact request. That workflow *is* the project.

## Phase 7 — Break it properly (3 days) ★ the most important phase

Random `sleep` calls are worthless: there's nothing to diagnose, because the answer is "the code sleeps." Build faults with a **causal chain**, where the symptom and the cause are in different places.

Put each behind a runtime flag (env var or an admin endpoint) so you can reproduce failures on demand — including live in an interview.

**Fault 1 — Missing index.** `SELECT * FROM inventory WHERE sku = $1` with no index on `sku`. With 200k rows, Postgres sequential-scans. Symptom: inventory p99 climbs *with data volume and concurrency*, not with a fixed delay. Fix: `CREATE INDEX`. Measure before and after; put both numbers in the README.

**Fault 2 — N+1 queries.** Orders fetches an order, then loops over items issuing one query each. Symptom: latency scales with order size, and the trace shows 47 tiny sibling SQL spans instead of one. This one is visually obvious in a trace waterfall and invisible in metrics — a perfect demonstration of *why traces exist*.

**Fault 3 — Connection pool exhaustion.** `db.SetMaxOpenConns(3)`, then apply 50 concurrent users. Symptom: latency rises sharply, but the database itself is idle — low CPU, fast queries. Time is spent *waiting for a connection*, not executing. This teaches saturation vs utilisation (the USE method), and the trap of blaming the database because the symptom appears there.

**Fault 4 — Slow tail plus a missing timeout.** Inventory delays 5% of requests by 5 seconds; Orders calls it with no client timeout. Symptom: the mean barely moves, p99 explodes, goroutines pile up, and eventually Orders degrades even though Orders has no bug. This is cascading failure, and it's the story that makes a reviewer nod.

**Fault 5 — Controllable error rate.** An env var or admin endpoint that makes Inventory return HTTP 500 on a configurable percentage of requests: `POST /admin/fault?errors=3` sets it to 3%. Unlike faults 1–4 this one isn't a realistic bug and isn't for investigation — it's an instrument. You need a *known, dial-able* error rate to verify burn-rate alert thresholds in Phase 14, because the whole point of that phase is proving your alert fires at the speed the math predicts. Build it now; it's twenty lines.

**Done when:** each fault can be turned on with one command and produces a distinct, recognisable signature in your dashboards.

## Phase 8 — Dashboards that answer questions (1 day)

Do not build five dashboards. Build two good ones.

**Dashboard 1 — Service health (RED):** request rate by service and route, error ratio, p50/p95/p99 latency with exemplars enabled. One row per service.

**Dashboard 2 — Dependencies (USE):** DB query duration by operation, connection pool in-use vs idle vs waiting, downstream call latency from each caller's perspective.

The design rule: a panel exists to answer a question. If you can't state the question a panel answers, delete it. "Is the service healthy?" "Is it fast?" "Where is the time going?" "Are we saturated?" Four questions, four panels.

**Stretch, if time allows:** define an SLO (99% of `/orders` under 300 ms) and graph error-budget burn rate. Very few candidates do this, and it maps directly to how SRE teams — Datadog's included — actually operate.

## Phase 9 — Load testing with k6 (½ day)

Write scenarios: steady baseline, ramp, and a burst. Constant load is what makes fault 3 visible — pool exhaustion only appears under concurrency.

```javascript
export const options = {
  stages: [
    { duration: '2m', target: 20 },
    { duration: '5m', target: 20 },
    { duration: '2m', target: 100 },  // burst
    { duration: '2m', target: 0 },
  ],
  thresholds: { http_req_duration: ['p(99)<500'] },
};
```

**Done when:** you can run a 10-minute load test, flip a fault mid-run, and watch the signature appear on the dashboard in real time.

## Phase 10 — The investigation write-ups (2 days) ★ the actual differentiator

This is what the reviewer was pointing at, and it's the part almost nobody does. For each fault, write a document in `docs/investigations/` structured like a real incident review:

1. **Symptom** — what a user or an alert would report. "p99 on POST /orders went from 90 ms to 3.2 s at 14:02."
2. **First look** — the dashboard screenshot. What narrowed the search, and what was *ruled out*.
3. **Trace analysis** — screenshot of a slow trace. Where the time actually went.
4. **Logs** — the correlated log lines, with the trace ID visible.
5. **Root cause** — the actual mechanism, stated precisely.
6. **Fix** — the diff.
7. **Verification** — before/after numbers from the same dashboard, from the same load profile.
8. **What I'd add** — the metric, span attribute, or alert that would have caught this in one minute instead of twenty.

Point 8 is the most senior thing in the whole document. It shows you understand that instrumentation is designed iteratively, in response to the failures you actually experience.

Write three of these. Three real investigations beat any amount of extra infrastructure.

## Phase 11 — Kubernetes, locally first (2 days)

Only now, with everything working on Compose. Use `kind` or `k3d`. Every hour here is free; every hour of the same work on EKS is billed.

Deploy your three services with:

- **Liveness and readiness probes**, and understand the difference — readiness removes a pod from the Service's endpoints, liveness *restarts the container*. Confusing them is a classic outage: a slow-but-healthy pod under load fails its liveness probe, gets killed, its traffic shifts to the remaining pods, which now also fail, and the whole deployment restart-loops itself to death. Readiness for "not ready yet", liveness only for "genuinely wedged".
- **Resource requests and limits.** Requests drive scheduling; limits drive throttling and OOM-kills. Set a CPU request, and be careful with CPU *limits* — they cause throttling that looks exactly like application latency in your dashboards. That's a great thing to point out in an interview.
- **`securityContext`** with `runAsNonRoot: true`, `readOnlyRootFilesystem: true`, and dropped capabilities.

Install the observability stack with Helm: `kube-prometheus-stack` (Prometheus + Grafana + Alertmanager + operator), `grafana/tempo`, `grafana/loki`.

**One current gotcha:** the old `loki-stack` chart ships Promtail, which reached end-of-life on 2 March 2026. Use **Grafana Alloy** as your log collector instead. If you install `loki-stack` because a 2023 tutorial told you to, you're deploying a dead component — and a reviewer who works in this space will notice.

Use **`ServiceMonitor`** resources for scraping rather than hand-editing `prometheus.yml`. The reason matters: it's Kubernetes-native service discovery, so a new pod is scraped automatically the moment it's Ready, with no config change and no reload. Static scrape configs don't survive autoscaling.

**Done when:** the full stack runs on `kind`, k6 hits it through a port-forward, and your dashboards look identical to the Compose version.

## Phase 12 — CI/CD: build, scan, push, deploy (1 day)

Four stages. Build them in this order, testing each before adding the next.

### 12.1 CI — lint, test, build

```yaml
- run: go vet ./...
- run: golangci-lint run
- run: go test ./... -race
- run: docker build -t $IMAGE:${{ github.sha }} .
```

Tag with the commit SHA, not just `latest`. `latest` is not a version — you cannot roll back to it, and you cannot tell which code is running in the cluster. Push both: `sha-<commit>` for traceability, `latest` for convenience.

### 12.2 Trivy — scan the image

Trivy scans your built image for known CVEs in OS packages and Go dependencies:

```yaml
- name: Scan image
  uses: aquasecurity/trivy-action@master
  with:
    image-ref: ${{ env.IMAGE }}:${{ github.sha }}
    format: sarif
    output: trivy-results.sarif
    severity: CRITICAL,HIGH
    exit-code: '1'          # fail the build on CRITICAL/HIGH

- name: Upload to GitHub Security
  uses: github/codeql-action/upload-sarif@v3
  with:
    sarif_file: trivy-results.sarif
```

Two decisions worth being able to defend:

- **`exit-code: 1` makes the scan a gate, not a report.** Most people set `exit-code: 0`, get a wall of green ✓ with 40 unfixed CVEs behind it, and call it "security scanning." Failing the build is the point.
- **The best way to pass the scan is to have less to scan.** Build a distroless or scratch image: multi-stage Dockerfile, `CGO_ENABLED=0 go build`, copy the static binary into `gcr.io/distroless/static`. A Go binary on distroless typically has *zero* OS-package CVEs, because there are no OS packages. That's a much better answer than suppressing findings, and it's a genuine engineering decision you can talk about.

### 12.3 Docker Hub push

```yaml
- uses: docker/login-action@v3
  with:
    username: ${{ secrets.DOCKERHUB_USERNAME }}
    password: ${{ secrets.DOCKERHUB_TOKEN }}   # access token, NOT your password
```

Generate an access token in Docker Hub settings and scope it to read/write. Never put your account password in a secret.

Push only on `main`, not on pull requests — otherwise every PR from a fork tries to publish to your registry.

### 12.4 GitHub OIDC → AWS (the part the other repo faked)

This is the piece worth doing properly, because it's the exact claim that got called out. Static `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` in GitHub secrets are long-lived credentials: if they leak, they work until you notice and rotate them. OIDC replaces them with a token GitHub mints per workflow run, which AWS exchanges for credentials that expire in an hour and are scoped to one repository.

**Step 1 — Create the identity provider in AWS.** IAM → Identity providers → Add provider → OpenID Connect.

- Provider URL: `https://token.actions.githubusercontent.com`
- Audience: `sts.amazonaws.com`

**Step 2 — Create a role with this trust policy.** The `sub` condition is the security boundary — it's what stops any other GitHub repository in the world from assuming your role:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "Federated": "arn:aws:iam::<ACCOUNT_ID>:oidc-provider/token.actions.githubusercontent.com"
    },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "token.actions.githubusercontent.com:aud": "sts.amazonaws.com"
      },
      "StringLike": {
        "token.actions.githubusercontent.com:sub": "repo:<YOUR_USER>/<YOUR_REPO>:ref:refs/heads/main"
      }
    }
  }]
}
```

Pin `sub` to your repo **and branch**. `repo:you/repo:*` also works but allows any branch, including one opened by a pull request — which is how people accidentally hand AWS access to anyone who can open a PR.

**Step 3 — The workflow.** The `permissions` block is mandatory and is the single most common reason OIDC "doesn't work":

```yaml
permissions:
  id-token: write     # WITHOUT THIS, NO OIDC TOKEN IS ISSUED
  contents: read

steps:
  - uses: aws-actions/configure-aws-credentials@v4
    with:
      role-to-assume: arn:aws:iam::<ACCOUNT_ID>:role/GitHubActionsEKSDeployRole
      aws-region: us-east-1

  - run: |
      aws eks update-kubeconfig --name obs-platform --region us-east-1
      kubectl set image deployment/orders orders=$IMAGE:${{ github.sha }} -n observability
      kubectl rollout status deployment/orders -n observability --timeout=120s
```

**Step 4 — Give the IAM role access *inside* the cluster.** This is the step that catches everyone, and it's worth understanding rather than copy-pasting past. AWS IAM authenticates you to the EKS *API*; it does not authorise you inside Kubernetes. An IAM role with full `AmazonEKSClusterPolicy` still gets `error: You must be logged in to the server (Unauthorized)` from `kubectl`, because Kubernetes RBAC is a separate system. You map the role to a Kubernetes identity with an EKS access entry:

```bash
aws eks create-access-entry \
  --cluster-name obs-platform \
  --principal-arn arn:aws:iam::<ACCOUNT_ID>:role/GitHubActionsEKSDeployRole \
  --type STANDARD

aws eks associate-access-policy \
  --cluster-name obs-platform \
  --principal-arn arn:aws:iam::<ACCOUNT_ID>:role/GitHubActionsEKSDeployRole \
  --access-scope type=namespace,namespaces=observability \
  --policy-arn arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy
```

(Older clusters used the `aws-auth` ConfigMap; access entries are the current mechanism and are what you should use.)

Scope it to the `observability` namespace with `AmazonEKSEditPolicy`, not `AmazonEKSClusterAdminPolicy`. Least privilege is free here, and "why does your deploy role have cluster-admin?" is an easy question to get asked and an easy one to have already answered.

**Done when:** you can revoke every static AWS credential from your GitHub secrets and the deploy still works. Test it by actually deleting them.

## Phase 12.5 — EKS, for four days (1 day of work)

Now, and only now, take it to AWS.

### Cluster config that doesn't waste money

```yaml
# cluster.yaml
apiVersion: eksctl.io/v1alpha5
kind: ClusterConfig
metadata:
  name: obs-platform
  region: us-east-1        # cheaper than eu-west-3, latency irrelevant for screenshots
  version: "1.31"          # stay on a version in STANDARD support

vpc:
  nat:
    gateway: Disable       # saves ~$33/month; nodes go in public subnets

managedNodeGroups:
  - name: workers
    instanceType: m7i-flex.large    # 2 vCPU / 8 GB — free-plan eligible
    desiredCapacity: 2
    spot: true                      # ~70% cheaper
    volumeSize: 30
```

```bash
eksctl create cluster -f cluster.yaml     # ~15 min
```

Four things in that file are cost decisions, and you should know why each one is there:

1. **`nat.gateway: Disable`** — eksctl's default NAT Gateway costs $0.045/hour plus $0.045/GB, for a demo cluster that doesn't need private subnets. This is the single biggest avoidable line item.
2. **`spot: true`** — ~70% off. If a node gets reclaimed mid-demo, you get to watch Kubernetes reschedule your pods, which is a free lesson.
3. **`version` in standard support** — a cluster on an out-of-support Kubernetes version jumps from $0.10/hr to $0.60/hr automatically, with no warning. Six times the price for doing nothing.
4. **No `type: LoadBalancer` Service** — use `kubectl port-forward` for your screenshots. An ALB is ~$0.025/hr plus LCU charges for zero added signal.

Expected cost for a four-day run: control plane $9.60 + spot nodes ~$6 + EBS ~$0.70 + public IPv4 ~$1 ≈ **$17**, against $200 of free-tier credits.

### Before you create anything

Set an **AWS Budget alert at $10**. Not after. (Bonus: creating a budget is one of the five $20 credit-earning onboarding activities, so AWS pays you to install your own guardrail.)

### Deletion checklist — read this before you `create`

`eksctl delete cluster` only removes what CloudFormation created. Anything Kubernetes created through the cloud controller — a `type: LoadBalancer` Service, a PersistentVolumeClaim — was not made by the stack, so deletion either hangs or silently orphans it. An abandoned NAT Gateway plus Elastic IP bills ~$35/month into a console you've stopped looking at.

After `eksctl delete cluster`, open the console **in the right region** and confirm zero of each:

- [ ] EC2 instances
- [ ] NAT Gateways
- [ ] Elastic IPs
- [ ] Load Balancers (EC2 → Load Balancers)
- [ ] EBS volumes (unattached)
- [ ] CloudFormation stacks (`eksctl-*`)

### What to capture while it's alive

Screenshots of: `kubectl get pods -n observability`, the RED dashboard under k6 load, a distributed trace spanning all three services, a log line with its `trace_id`, the GitHub Actions run showing a successful OIDC deploy, and the Trivy scan result. Then destroy it.

**A note on honesty, drawn directly from the review of that other repo:** the README claimed GitHub OIDC while the workflow used static AWS keys. Whatever your README says must be true of your code. A reviewer opens `.github/workflows/` and diffs it against your claims in thirty seconds — and a false claim is worse than a missing feature, because it makes them distrust everything else on the page. If you skip a phase, write that you skipped it.

## Phase 14 — SLO and error-budget engine (3–4 days, AFTER you ship)

Do not start this until the repo is public with three investigation write-ups. This is the layer you add *after* applying, so the deep-dive has something recent to open with.

It needs no new services and no new infrastructure — the SLI is the counter you already built in Phase 2.

**The vocabulary, in order.** An **SLI** is the measurement (percentage of requests that succeeded). An **SLO** is the target for it (99.9% over 30 days). The **error budget** is the remainder: 0.1% of requests are allowed to fail. On 10 million requests that's 10,000 failures you're permitted to spend — on a risky deploy, a migration, an experiment. The framing matters because it turns reliability from a binary into an allowance.

**Burn rate** is how fast you're spending it, relative to the pace that would exactly exhaust the budget by the end of the window:

```
burn rate = observed error rate / allowed error rate
```

With a 99.9% SLO the allowed rate is 0.1%, so 1% observed errors is burn rate 10 — budget gone in 3 days instead of 30. One number gives you severity and urgency together.

**Multi-window, multi-burn-rate.** Alerting at burn rate > 1 fires on a single bad second. Alerting only at budget exhaustion tells you after the damage. So you run several rules at once, from Google's standard set for a 30-day window:

| Burn rate | Long window | Meaning | Action |
|---|---|---|---|
| 14.4× | 1 hour | 2% of the month's budget in an hour | Page |
| 6× | 6 hours | 5% in six hours | Page |
| 3× | 1 day | 10% in a day | Ticket |
| 1× | 3 days | 10% in three days | Ticket |

Check the first row yourself so you trust it: one hour is 1/720 of a 30-day month, and 14.4 × 1/720 = 2%. Being able to derive that on demand is most of this phase's interview value.

Each rule ANDs a long window with a short one (typically long ÷ 12 — the 1-hour rule also checks the last 5 minutes), and **both** must exceed the threshold. Without the short window, an incident that started an hour ago and is already fixed keeps the 1-hour average high and the alert keeps screaming at a healthy service. The short window asks "is this still happening *now*", which is what makes alerts turn off. Most people miss this detail; explaining it is a strong signal.

**Build order:**

1. Recording rules for the SLI ratio at each window (5m, 30m, 1h, 6h, 1d, 3d). Precomputed, because the alert queries would otherwise re-scan long ranges every evaluation.
2. Burn rate expressions: `(1 - ratio) / (1 - 0.999)`.
3. Four alert rules, each ANDing long and short windows.
4. Alertmanager routing — page-severity to one receiver, ticket-severity to another. You get Alertmanager free with `kube-prometheus-stack` in Phase 11; on Compose you add the container yourself.
5. Dashboard three: budget remaining, current burn rate, projected exhaustion date.
6. **Proof.** Dial fault 5 to 3% errors and screenshot the 14.4× alert firing in under two minutes while the 1× rule stays quiet. Without this the phase is a config file; with it, it's a demonstration.

**Write the rules by hand.** There's a version of this project that reads SLO definitions from YAML and generates the rules — skip it on the first pass. Deriving each expression yourself is where the understanding comes from, and the understanding is what gets tested. A generator emitting rules you can't explain is the easier half of the work and the worthless half. Add it later if you want the tool to be reusable.

**Optional extension — measured detection time.** Once alerts exist, a ~150-line Go runner can flip each Phase 7 fault, poll the Prometheus API until the expected alert enters `firing`, record the delta, and tear the fault down. That gives you an MTTD table per fault class. Include the failures — "DB latency: not detected, because the latency SLI used the mean instead of a quantile; fixed, then 3m20s" is the single most valuable line you can put in front of an SRE interviewer, because finding a gap in your own monitoring and closing it with evidence *is* the job.

## Phase 13 — Stretch: send it to Datadog

If you have time and an account: add the Datadog exporter to the Collector config and ship the same telemetry to Datadog APM alongside Tempo. Then write a short comparison — what was easier, what was harder, what the hosted product gives you that the OSS stack doesn't.

For a Datadog application specifically, that document may be the single highest-value page in the repo. It shows you've used the product, thought about it critically, and can articulate the trade-offs.

---

# PART FOUR — PRESENTING IT

## The README

Structure it as: what this is (2 sentences) → architecture diagram → the correlation workflow with three screenshots → **links to the investigation write-ups** → how to run it → what's deliberately not included.

That last section matters more than it sounds. "Redis and multi-region were out of scope; I chose depth on instrumentation and failure analysis instead" reads as engineering judgement. Silence on scope reads as an unfinished project.

Be precise about what's real. If sampling is at 100% because traffic is tiny, say so, and say what you'd change at 100k req/s.

## The project deep-dive round

Datadog's format includes a ~35-minute deep-dive on a project you choose. Prepare answers to these, out loud:

- Walk me through what happens to a single request end to end.
- Why a histogram and not a summary? How do you compute p99 across ten pods?
- What stops this from working at 100,000 requests per second?
- Tell me about a bug you found using your own telemetry.
- What would you instrument differently if you started again?
- Where would this system's observability fail to help you?

That last one is where the strongest candidates separate themselves. Good answers: a fault in the network *between* services shows up as latency in the caller with no corresponding server span; a slow client shows nothing at all server-side; 100% CPU on the node makes every span look slow and points at nothing. Knowing the limits of your own tooling is a senior trait.

## Common failure modes to avoid

- Claiming things the code doesn't do. Fatal.
- Screenshots of empty dashboards. Generate load first, always.
- `latency` and `latency_seconds` in the same repo. Follow Prometheus naming: `_total` for counters, base units (seconds, bytes), `_seconds` suffix on the histogram.
- Instrumenting *everything*. A well-chosen set of metrics beats an exhaustive one, and it's a judgement call reviewers can see.

---

# PART FIVE — THREE-WEEK SCHEDULE

| Week | Phases | Where it runs | You end the week with |
|---|---|---|---|
| 1 | 0–4 | Laptop (Compose) | Three services, RED metrics, JSON logs, first trace |
| 2 | 5–8 | Laptop (Compose) | Real distributed traces, full correlation, four faults, two dashboards |
| 3 | 9–11 | Laptop (kind) | Load tests, three written investigations, running on Kubernetes |
| 4 (short) | 12–12.5 | **AWS, 4 days** | CI with Trivy + Docker Hub, OIDC deploy, EKS screenshots, README |
| — | **Ship publicly. Apply.** | — | — |
| after | 13–14 | Laptop | Datadog comparison, SLO engine + burn-rate alerts, MTTD table |

Week 4 is four days, not seven, and it's the only part that costs money. Create the cluster on day 1, deploy, load-test, screenshot, write the README, destroy on day 4.

If week 3 slips, drop the EKS week before dropping the investigations. **The write-ups are the project. Everything else is setup for them.** A README that says "runs on kind; EKS deployment planned" is honest and fine. A README that claims EKS when there's no cluster config is the mistake you're trying not to repeat.

---

# PART SIX — RESOURCES PER PHASE

Watch or read each one **when you reach that phase**, not upfront. Consuming all of this before writing code is how projects die at Phase 0.

A caveat on videos generally: observability tooling moves fast, and a 2020–2022 video will show an OpenTelemetry API that no longer compiles. Use videos to build the mental model, then write code from the current written docs. Where a video is dated, I've flagged it.

### Phase 2 — Prometheus metrics in Go

- **Video:** [How to Set Up Prometheus & Grafana: Monitoring an API with Golang](https://www.youtube.com/watch?v=pP2DKCKR4CQ) — instruments a Go API with latency metrics. The closest video to what you're building in this phase.
- **Video:** [Visualizing metrics with Grafana](https://www.youtube.com/watch?v=QT66dU_h9lo) — the official Prometheus docs tutorial video. Short.
- **Read (authoritative):** [Instrumenting a Go application for Prometheus](https://prometheus.io/docs/guides/go-application/) — the official guide. Do this one hands-on.
- **Read:** [Instrumenting & Monitoring Go Apps with Prometheus](https://betterstack.com/community/guides/monitoring/prometheus-golang/) — covers CounterVec, labels, and middleware in more depth, with a companion repo.
- **Read:** Prometheus docs → *Metric types* and *Naming conventions*. Fifteen minutes, prevents most beginner mistakes.

### Phase 4–5 — OpenTelemetry tracing and propagation

- **Video (concepts first):** [Distributed Tracing Explained: OpenTelemetry & Jaeger](https://www.youtube.com/watch?v=Oa-zqv-EBpw) — opens with exactly the problem you're solving: users report 8-second responses while your metrics look fine. Watch this before writing any tracing code.
- **Video:** [Implementing Distributed Tracing in Golang with OpenTelemetry](https://www.youtube.com/watch?v=7C59tF2sjuo)
- **Video:** [Practical introduction to OpenTelemetry tracing — Nicolas Frankel](https://www.youtube.com/watch?v=_vVh1dGGqKY) — conference talk, strong on the *why*.
- **Read (do this hands-on):** [Practical Tracing for Go Apps with OpenTelemetry](https://betterstack.com/community/guides/observability/opentelemetry-go/) — SDK setup, `otelhttp`, span creation, with Docker Compose.
- **Read:** [Distributed Tracing with OpenTelemetry in Go: A Practical Guide](https://dev.to/young_gao/distributed-tracing-with-opentelemetry-a-practical-guide-for-go-services-pep) — covers `otelhttp.NewHandler` and `otelhttp.NewTransport`, i.e. the propagation piece that Phase 5 is entirely about.
- **Read:** W3C Trace Context spec, §3 only (the header format). Twenty minutes and propagation stops being magic.
- **Dated but still good on fundamentals:** [OpenTelemetry Deep Dive: Golang — Ted Young](https://www.youtube.com/watch?v=yQpyIrdxmQc) (2020 — the API has changed, don't copy the code).

### Phase 6 — Correlation

- **Read (authoritative, do this exactly):** [Configure trace to logs correlation](https://grafana.com/docs/grafana/latest/datasources/tempo/configure-tempo-data-source/configure-trace-to-logs/) — Grafana's official guide. Note their own warning: you must configure **both** the Tempo side and the Loki derived field. Configuring only Tempo is the most common mistake, and the symptom is that clicking a log line does nothing.
- **Read:** [How to Correlate Logs and Traces with Loki and Tempo](https://oneuptime.com/blog/post/2026-01-21-loki-tempo-traces-correlation/view) — end-to-end walkthrough. Also the source of the Promtail end-of-life note.

### Phase 7 & 10 — Faults and investigations

- **Read (this is the single most relevant article to your whole project):** [How to fix performance issues using k6 and the Grafana LGTM Stack](https://grafana.com/blog/how-to-fix-performance-issues-using-k6-and-the-grafana-lgtm-stack/) — Grafana engineers load-test an instrumented app, find a bottleneck, and fix it. That is literally the workflow of your Phase 10 write-ups. Model your investigation documents on this structure.
- **Read:** Brendan Gregg on the **USE method** — for fault 3 (pool exhaustion), and for the utilisation-vs-saturation distinction.
- **Read:** Google SRE Book, ch. 4 (SLOs) and ch. 6 (Monitoring Distributed Systems). Interview vocabulary lives here.

### Phase 9 — k6

- **Video:** [k6 Beginner Tutorial — Getting Started with Performance Testing](https://www.classcentral.com/course/youtube-k6-beginner-tutorial-1-getting-started-522373) — install, first script, HTML reports.
- **Video (later, optional):** [Distributed load testing using Kubernetes with k6](https://www.youtube.com/watch?v=5d5zxsGz8L4) — k6 Office Hours, for the k6 operator. Beyond what you need, but useful if load generation from your laptop becomes the bottleneck.
- **Read:** k6 docs → *Test types* and *Thresholds*.

### Phase 11 — Kubernetes and EKS

- **Video:** [AWS EKS Tutorial for Beginners (2026)](https://www.youtube.com/watch?v=MlDJfLVycIA) — most recent of the beginner EKS walkthroughs, so least likely to show a deprecated flow.
- **Video:** [AWS EKS Tutorial for Beginners — Build a Cluster in 30 Minutes](https://www.youtube.com/watch?v=f2Su3DxSEOc)
- **Video:** [Deploy Your First Kubernetes Cluster on EKS](https://www.youtube.com/watch?v=dAfJ9uUmbjU)
- **Read (best written reference, and current):** [How to Create AWS EKS Cluster Using eksctl](https://devopscube.com/create-aws-eks-cluster-eksctl/) — covers spot vs on-demand node groups, add-ons, and adding IAM roles to the cluster. Use this over the videos for the actual config file.

### Phase 12 — CI/CD, Trivy, OIDC

- **Read (do this one step by step):** [How to Configure GitHub Actions OIDC with AWS](https://devopscube.com/github-actions-oidc-aws/) — identity provider, trust policy, `permissions: id-token: write`, and then an EKS deployment. Matches Phase 12.4 exactly.
- **Read:** [Build a Secure CI/CD Pipeline for Amazon EKS Using GitHub Actions and AWS OIDC](https://dev.to/dhayv/build-a-secure-cicd-pipeline-for-amazon-eks-using-github-actions-and-aws-oidc-3b0m) — second pass over the same material from a different angle; useful when the first one doesn't click.
- **Read:** [Executing kubectl commands on an EKS cluster via GitHub Actions](https://medium.com/@sachinpujeri12/executing-kubectl-commands-on-eks-cluster-via-github-actions-7d0ce4dabb69) — specifically for the trap in Phase 12 step 4: why an IAM role with EKS permissions still gets `Unauthorized` from `kubectl`.
- **Read:** Trivy docs → *GitHub Actions integration*, and the `trivy-action` README for the `exit-code` and SARIF options.

### Optional background

**Charity Majors et al., *Observability Engineering*** (chapters 1–4). Not required, but it's where most of the vocabulary in Part One comes from, and it's the book people at observability companies have read.
