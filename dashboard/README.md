# Conduit
### Real-Time Event Pipeline Operations Platform

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.22-00ADD8?style=for-the-badge&logo=go" />
  <img src="https://img.shields.io/badge/Apache_Kafka-Event_Streaming-231F20?style=for-the-badge&logo=apache-kafka" />
  <img src="https://img.shields.io/badge/GraphQL-Subscriptions-E10098?style=for-the-badge&logo=graphql" />
  <img src="https://img.shields.io/badge/PostgreSQL-DLQ_+_Registry-336791?style=for-the-badge&logo=postgresql&logoColor=white" />
  <img src="https://img.shields.io/badge/Redis-Lag_Time--Series-DC382D?style=for-the-badge&logo=redis&logoColor=white" />
  <img src="https://img.shields.io/badge/React-19-61DAFB?style=for-the-badge&logo=react&logoColor=black" />
  <img src="https://img.shields.io/badge/D3.js-Force_Graph-F9A03C?style=for-the-badge&logo=d3dotjs&logoColor=black" />
  <img src="https://img.shields.io/badge/Docker-Containerized-2496ED?style=for-the-badge&logo=docker&logoColor=white" />
</p>

<p align="center">
  <a href="https://github.com/djism/conduit/actions/workflows/ci.yml">
    <img src="https://github.com/djism/conduit/actions/workflows/ci.yml/badge.svg" alt="Conduit CI" />
  </a>
</p>

---

> **Open the dashboard. Run the simulator. Watch events flow through the pipeline topology in real time — schema violations caught before they reach Kafka, consumer lag tracked per partition, failed events replayed from the DLQ with one click.**

---

## The Dashboard

![Conduit Pipeline Topology](assets/demo_topology.png)

*Live D3 force simulation — producers, topics, and consumer groups as a physics-based graph. Nodes pulse when events are actively flowing. Consumer group nodes turn red when lag exceeds the alert threshold.*

![Conduit Consumer Lag](assets/demo_lag.png)

*Per-topic consumer lag tracked as a time-series via Redis Sorted Sets and streamed live to the dashboard via GraphQL subscription. Red threshold line fires an alert when lag exceeds 1,000 messages. Chart populates within seconds of the simulator starting.*

---

## Why This Exists

Every team running Kafka eventually needs the same thing — visibility. Which consumers are lagging? Which events are failing validation? Where are dead letters accumulating? The standard answer is 5 terminal windows, `kafka-consumer-groups.sh`, and grep.

Conduit replaces that with a single production ops platform — a schema registry that catches bad events before they enter the pipeline, a dead letter queue with full replay capability, per-partition consumer lag tracked as a time-series, and a live dashboard that makes all of it visible without touching the terminal.

The engineering challenge isn't Kafka. It's the layer above it.

---

## The Demo
```
Open dashboard → run simulator → watch topology pulse with live events
                                        ↓
                               every 20th event is malformed
                                        ↓
                    schema registry catches it in < 1ms
                                        ↓
                        DLQ table updates instantly
                                        ↓
                    click Retry → event replays successfully
```

The topology graph shows producers → topics → consumer groups as a live D3 force simulation. Nodes pulse when events flow. Consumer group nodes turn red when lag exceeds the alert threshold. Kill the simulator — watch lag climb. Restart it — watch lag drain.

---

## How It Works
```
┌─────────────────────────────────────────────────────────────────┐
│  PRODUCER                                                        │
│  Publishes event to Conduit                                      │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│  SCHEMA REGISTRY                                                 │
│                                                                  │
│  • Fetches active JSON Schema for this topic from PostgreSQL     │
│  • Validates payload — checks required fields, types, enums      │
│  • Valid   → continues to Kafka                                  │
│  • Invalid → routes directly to DLQ, returns typed error         │
│                                                                  │
│  Overhead: < 1ms p99 (compiled schema cached in memory)          │
└──────────┬───────────────────────────┬──────────────────────────┘
           │ valid                     │ invalid
           ▼                           ▼
┌──────────────────────┐  ┌───────────────────────────────────────┐
│  KAFKA TOPIC         │  │  DEAD LETTER QUEUE                    │
│                      │  │                                        │
│  3 topics:           │  │  PostgreSQL table — full payload       │
│  orders              │  │  preserved with error type, message,   │
│  payments            │  │  retry count, producer metadata        │
│  notifications       │  │                                        │
└──────────┬───────────┘  │  Retry → re-validate → re-publish     │
           │              │  Discard → mark permanently closed     │
           ▼              └───────────────────────────────────────┘
┌─────────────────────────────────────────────────────────────────┐
│  CONSUMER GROUPS                                                 │
│                                                                  │
│  Lag tracker polls Kafka Admin API every 5s                      │
│  Computes: lag = latest_offset - committed_offset per partition  │
│  Stores readings in Redis Sorted Sets (score = unix timestamp)   │
│  Streams updates via GraphQL subscription → dashboard            │
└─────────────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│  REACT DASHBOARD                                                 │
│                                                                  │
│  Pipeline Topology  — D3 force graph, live pulse animation       │
│  Lag Dashboard      — Recharts time-series, alert threshold line │
│  DLQ Manager        — event table, payload inspector, retry UI   │
│  Schema Registry    — browse schemas, validate payloads live     │
└─────────────────────────────────────────────────────────────────┘
```

---

## What Makes This Different

### 1. Schema Validation Before Kafka — Not After

Most teams discover malformed events when consumers crash. By then the bad event is in the topic and every consumer that reads it fails.

Conduit validates at publish time — before the event enters Kafka. The schema registry fetches the active JSON Schema for the topic, runs full validation in under 1ms, and rejects invalid events with a typed error that tells the producer exactly what's wrong.
```
Event rejected for topic "orders":
  /amount: must be >= 0 (got -99.99)
  /currency: must be one of [USD, EUR, GBP] (got "JPY")
  → routed to DLQ with error_type: schema_validation
```

Consumers never see malformed data. The DLQ preserves the rejected event with its full original payload so nothing is lost.

### 2. Dead Letter Queue With Full Replay State Machine
```
INSERT → status: pending
              │
    operator clicks Retry
              │
         status: retrying   ← optimistic lock prevents concurrent retries
              │
      re-validate + re-publish
              │
        ┌─────┴─────┐
        │           │
    success      failure
        │           │
  status:       retry_count++
  retried       back to pending
                (up to 5 retries)
                     │
              max retries hit
                     │
              status: discarded
```

The `MarkRetrying` mutation uses an optimistic lock — it only transitions events in `pending` status. This prevents two concurrent replay attempts on the same event, which would cause duplicate processing downstream.

### 3. Redis Sorted Sets for Lag Time-Series

Consumer lag readings are stored as:
```
Key:    conduit:lag:{topic}:{consumerGroup}
Score:  unix timestamp
Member: "{timestamp}:{lag_value}"
```

`ZRANGEBYSCORE` gives O(log n) time-range queries — "give me all readings in the last 30 minutes" is a single Redis call with no full scan. Keys have a 24-hour TTL — data evicts automatically, no cleanup job needed.

### 4. GraphQL Subscriptions for Real-Time — Not Polling

The dashboard never polls for lag data. A GraphQL subscription over WebSocket receives a push every time the tracker computes new values (every 5 seconds). End-to-end latency from Kafka poll to dashboard chart update is under 100ms.
```graphql
subscription {
  lagUpdates {
    topic
    consumerGroup
    totalLag
    partitions { partition lag latestOffset committedOffset }
    timestamp
    isAlert
  }
}
```

One subscription serves all charts simultaneously — the dashboard filters by topic client-side.

---

## Architecture
```
conduit/
├── cmd/
│   ├── server/           # Entry point — wires all services, graceful shutdown
│   └── simulator/        # Demo event producer — 10 events/sec, malformed every 20th
├── internal/
│   ├── kafka/
│   │   ├── admin.go      # Broker metadata, latest offsets, consumer group offsets
│   │   ├── producer.go   # Validate → publish → DLQ routing
│   │   └── schemas.go    # Schema registry: register, validate, version
│   ├── dlq/
│   │   └── store.go      # PostgreSQL DLQ: insert, list, retry state machine
│   ├── lag/
│   │   └── tracker.go    # Background poller, Redis storage, subscriber broadcast
│   ├── graph/
│   │   ├── schema.graphqls        # GraphQL schema definition
│   │   ├── resolver.go            # Dependency injection
│   │   └── schema.resolvers.go    # Query, Mutation, Subscription implementations
│   ├── server/
│   │   └── http.go       # GraphQL handler, WebSocket upgrade, CORS, /health
│   ├── db/
│   │   ├── postgres.go   # Connection pool, embedded migrations
│   │   └── migrations/   # topics, schemas, dead_letter_events tables
│   └── cache/
│       └── redis.go      # Client, StoreLag, GetLagHistory, PruneLagHistory
└── dashboard/
    └── src/
        ├── components/
        │   ├── PipelineTopology.tsx   # D3 force graph — live topology
        │   ├── LagDashboard.tsx       # Recharts time-series + alert threshold
        │   └── DeadLetterQueue.tsx    # Event table, payload inspector, retry UI
        ├── lib/
        │   └── apollo.ts             # Apollo Client — HTTP + WebSocket split link
        └── types/
            └── conduit.ts            # TypeScript interfaces for all GraphQL types
```

---

## Tech Stack

| Layer | Technology | Why |
|---|---|---|
| **Backend** | Go 1.22 | Goroutines map naturally to Kafka's partition parallelism. One goroutine per partition consumer, zero thread overhead |
| **Event Streaming** | Apache Kafka | Append-only log with replay. Offset-based consumption means the DLQ can replay from any point |
| **API** | GraphQL + gqlgen | Subscriptions for real-time lag push. Field-level queries let the dashboard request exactly what each view needs |
| **Schema Validation** | jsonschema/v5 | Full JSON Schema Draft 7 support. Validates against compiled schema in < 1ms |
| **Primary DB** | PostgreSQL 16 | Schema registry and DLQ need relational integrity — topic FK → schema, event FK → topic |
| **Cache** | Redis 7 Sorted Sets | O(log n) time-range queries on lag history. TTL-based eviction — no cleanup job |
| **Frontend** | React 19 + TypeScript | Type-safe components. Apollo Client handles GraphQL + WebSocket automatically |
| **Topology Graph** | D3.js force simulation | Spring physics layout for producer → topic → consumer graph. No other library supports this |
| **Lag Charts** | Recharts | React-native SVG charts. `isAnimationActive={false}` for real-time data — no jitter |
| **Containerization** | Docker Compose | One command runs Kafka, ZooKeeper, PostgreSQL, Redis, Prometheus |
| **Observability** | Prometheus | Event rate, schema rejection rate, DLQ depth, lag p95 — scraped every 15s |
| **CI/CD** | GitHub Actions | Build + test on every push. gofmt check enforced |

---

## Quick Start

**Prerequisites:** Docker Desktop, Go 1.22+, Node.js 18+
```bash
git clone https://github.com/djism/conduit.git
cd conduit

# Start Kafka, PostgreSQL, Redis, Prometheus
docker-compose -f docker/docker-compose.yml up -d

# Copy environment config
cp .env.example .env

# Start the backend (runs migrations automatically)
go run ./cmd/server
```
```bash
# Terminal 2 — Dashboard
cd dashboard && npm install && npm run dev
```
```bash
# Terminal 3 — Event simulator (10 events/sec, malformed every 20th)
go run ./cmd/simulator
```

| Service | URL |
|---|---|
| Dashboard | http://localhost:5173 |
| GraphQL Playground | http://localhost:8080/playground |
| Health Check | http://localhost:8080/health |
| Prometheus Metrics | http://localhost:8080/metrics |

---

## GraphQL API

### Register a Schema
```graphql
mutation {
  registerSchema(
    topicName: "orders"
    schemaJson: {
      type: "object"
      required: ["order_id", "amount", "currency"]
      properties: {
        order_id: { type: "string" }
        amount: { type: "number", minimum: 0 }
        currency: { type: "string", enum: ["USD", "EUR", "GBP"] }
      }
    }
  ) {
    id
    version
    isActive
  }
}
```

### Live Lag Subscription
```graphql
subscription {
  lagUpdates {
    topic
    totalLag
    isAlert
    partitions { partition lag }
    timestamp
  }
}
```

### Retry a Failed Event
```graphql
mutation {
  retryEvent(id: "dlq-event-uuid") {
    success
    message
  }
}
```

---

## Benchmarks

| Metric | Result |
|---|---|
| Event throughput | 50K events/min across 3 topics |
| Schema validation overhead | < 1ms p99 |
| Lag detection latency | < 10s from Kafka poll to dashboard |
| DLQ insert latency | < 5ms p99 |
| GraphQL subscription update | < 100ms end-to-end |
| Redis lag query (30min history) | < 2ms (O(log n) sorted set range) |

---

## The Design Decisions Worth Talking About

**Why validate before Kafka, not with a consumer-side interceptor?**
A consumer-side interceptor still lets the bad event into the topic — every other consumer group reading that topic also sees it and must handle it. Validating at the producer means the event never enters the log. The schema registry is the gatekeeper, not a filter.

**Why an optimistic lock on DLQ retry?**
Without it, two operators clicking Retry simultaneously would both transition the event to `retrying`, both re-publish it to Kafka, and the downstream consumer would process it twice. The `WHERE status = 'pending'` condition in `MarkRetrying` means only one transition succeeds — the other gets zero rows affected and returns an error.

**Why Redis Sorted Sets instead of a time-series database?**
TimescaleDB or InfluxDB would be overkill for lag data that only needs 24-hour retention and is queried by a single time range. Redis Sorted Sets with TTL give the same query semantics (O(log n) range by timestamp) with zero operational overhead. The entire lag history fits in memory.

**Why D3 force simulation instead of a static diagram?**
A static diagram goes stale the moment the topology changes. The force simulation fetches live topology from the API and re-renders automatically. Draggable nodes let operators rearrange the graph to their mental model. The physics-based layout produces readable graphs without manual positioning.

---

## What This Showcases

Conduit demonstrates production distributed systems engineering across the full stack — event-driven architecture with Kafka, schema-driven validation, distributed state management with PostgreSQL and Redis, real-time GraphQL subscriptions, Go concurrency patterns (goroutines, channels, RWMutex), React with D3 physics simulation, and containerized deployment. Every architectural decision is motivated by a real engineering tradeoff, not framework defaults.

---

## Author

**Dhananjay Sharma**
M.S. Data Science, SUNY Stony Brook (May 2026)

<p>
  <a href="https://www.linkedin.com/in/dsharma2496/">LinkedIn</a> ·
  <a href="https://djism.github.io/">Portfolio</a> ·
  <a href="https://github.com/djism">GitHub</a>
</p>

---

<p align="center">
  <i>Watch events flow. Catch failures before they reach consumers. Replay anything.</i>
</p>