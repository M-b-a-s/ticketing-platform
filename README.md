# Ticketing Platform

A production-minded event ticketing backend written in Go. Organizers publish events and ticket types, customers reserve and pay for tickets (via Paystack), and gate attendants validate tickets at the door.

The goal of this project is not just to *work*, but to **stay correct under failure**: concurrent buyers, duplicate webhooks, crashed workers, and restarts must never oversell inventory, double-charge a customer, or issue duplicate tickets.

---

## Table of contents

1. [Goals and non-goals](#goals-and-non-goals)
2. [Correctness guarantees](#correctness-guarantees)
3. [Architecture](#architecture)
4. [Tech stack](#tech-stack)
5. [Project structure](#project-structure)
6. [Domain model and database schema](#domain-model-and-database-schema)
7. [Purchase lifecycle](#purchase-lifecycle)
8. [Key design decisions](#key-design-decisions)
9. [API overview](#api-overview)
10. [Getting started](#getting-started)
11. [Configuration](#configuration)
12. [Testing strategy](#testing-strategy)
13. [Observability](#observability)
14. [Security](#security)
15. [Deployment and CI/CD](#deployment-and-cicd)
16. [Operations runbook](#operations-runbook)
17. [Build roadmap](#build-roadmap)
18. [Out of scope (for now)](#out-of-scope-for-now)
19. [License](#license)

---

## Goals and non-goals

**Goals**

- Accurate inventory under heavy concurrency.
- Exactly-once *effects* (charge, ticket issuance) on top of at-least-once delivery (webhooks, jobs).
- Clear separation between order, payment, and ticket state.
- Survive restarts and crashes without losing orders or reservations.
- Observable: you can answer "how many orders, how many failed payments, how many stuck jobs" from dashboards.

**Non-goals**

- Microservices. This is a **modular monolith**: one codebase, two binaries (`api`, `worker`), clear module boundaries.
- Kafka, Kubernetes, or any infrastructure that is not justified by a measured need.
- Seat maps / assigned seating (see [Out of scope](#out-of-scope-for-now)).

---

## Correctness guarantees

These are the invariants the test suite exists to defend.

| # | Invariant | Enforced by |
|---|-----------|-------------|
| 1 | Tickets sold never exceed `total_quantity` | Atomic conditional `UPDATE` + `CHECK` constraints |
| 2 | A customer is not charged twice for the same order | Idempotency keys + unique payment reference |
| 3 | A webhook delivered N times issues tickets once | `webhook_events` unique key + idempotent fulfilment |
| 4 | A failed email job can be retried without touching the completed purchase | Outbox + idempotent consumers |
| 5 | An API restart loses no orders or reservations | All state in PostgreSQL, none in process memory |
| 6 | A worker crash does not corrupt business state | Transactions + `FOR UPDATE SKIP LOCKED` claiming + retries |
| 7 | A ticket can be checked in at most once | Atomic `UPDATE ... WHERE status = 'valid'` |
| 8 | Hundreds of simultaneous buyers are handled correctly | Concurrency and k6 load tests |

---

## Architecture

```
 Customers · Organizers · Admins · Gate attendants
                       │
                       ▼
        ┌──────────────────────────────┐
        │  API  (cmd/api, Chi)         │
        │  auth · RBAC · validation ·  │
        │  rate limiting · idempotency │
        └──────────────┬───────────────┘
                       │
        ┌──────────────▼───────────────┐        ┌──────────────────┐
        │  PostgreSQL (source of truth)│◄───────┤  Worker          │
        │  inventory · orders ·        │        │  (cmd/worker)    │
        │  payments · tickets · outbox │        │  outbox relay,   │
        └──────────────┬───────────────┘        │  expiry sweeper, │
                       │                        │  email, QR, etc. │
                       │ (optional, later)      └────────┬─────────┘
                       ▼                                 │
                 Redis + Asynq  ◄────────────────────────┘

 External: Paystack (payments, webhooks) · Email provider · Object storage
```

Principles:

- **PostgreSQL is the source of truth** for inventory, orders, payments, and tickets. Redis (if used) never decides availability.
- **Modules are logically separated** (`internal/<module>`) and talk through small interfaces, so any module can be extracted later if a real scaling need appears.
- **Side effects happen asynchronously**, driven by a transactional outbox.

---

## Tech stack

| Concern | Choice |
|---------|--------|
| Language | Go |
| HTTP router | Chi |
| Database | PostgreSQL |
| Driver / SQL | pgx + sqlc |
| Migrations | Goose |
| Background jobs | Outbox relay + worker pool (Postgres-backed); Redis + Asynq when needed |
| Payments | Paystack (NGN, amounts in kobo) |
| Auth | Secure HTTP-only session cookies, Argon2id password hashing |
| Validation | go-playground/validator |
| Logging | `log/slog` (JSON) |
| Metrics / dashboards | Prometheus + Grafana |
| Tracing | OpenTelemetry |
| Error monitoring | Sentry |
| Testing | Testify, Testcontainers, `go test -race` |
| Load testing | k6 |
| Containers / CI | Docker, GitHub Actions |
| Reverse proxy | Caddy or Nginx |
| API docs | OpenAPI |

---

## Project structure

```
ticketing-platform/
├── cmd/
│   ├── api/main.go          # HTTP server
│   └── worker/main.go       # background workers
├── internal/
│   ├── auth/                # sessions, password hashing, RBAC middleware
│   ├── users/
│   ├── events/
│   ├── tickets/             # ticket types and issued tickets
│   ├── inventory/           # reservation / release logic
│   ├── orders/
│   ├── payments/            # Paystack client, webhook handling
│   ├── checkin/
│   ├── notifications/       # email templates and senders
│   ├── outbox/              # outbox writer + relay
│   ├── jobs/                # job handlers
│   └── platform/
│       ├── config/
│       ├── database/
│       ├── logger/
│       ├── middleware/
│       └── telemetry/
├── migrations/              # Goose SQL migrations
├── sql/
│   ├── queries/             # sqlc query files
│   └── schema/
├── api/openapi.yaml
├── tests/
│   ├── integration/
│   ├── concurrency/
│   └── e2e/
├── deployments/docker/
├── scripts/
├── docker-compose.yml
├── Makefile
├── go.mod
└── README.md
```

Rules of thumb: handlers parse and respond, services hold business rules, repositories (sqlc) talk to the database. Modules never reach into each other's tables.

---

## Domain model and database schema

Money is stored as **integer minor units (kobo)**, never floats. All timestamps are `timestamptz`.

```sql
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TYPE user_role AS ENUM
  ('customer', 'organizer', 'event_admin', 'platform_admin', 'gate_attendant');

CREATE TABLE users (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  email         citext NOT NULL UNIQUE,
  password_hash text   NOT NULL,
  full_name     text   NOT NULL,
  role          user_role NOT NULL DEFAULT 'customer',
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE events (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organizer_id uuid NOT NULL REFERENCES users(id),
  title        text NOT NULL,
  description  text,
  venue_name   text NOT NULL,
  venue_address text,
  city         text,
  starts_at    timestamptz NOT NULL,
  ends_at      timestamptz NOT NULL,
  status       text NOT NULL DEFAULT 'draft'
               CHECK (status IN ('draft','published','cancelled','completed')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  CHECK (ends_at > starts_at)
);

CREATE TABLE ticket_types (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  event_id           uuid NOT NULL REFERENCES events(id),
  name               text NOT NULL,
  price_kobo         bigint NOT NULL CHECK (price_kobo >= 0),
  currency           char(3) NOT NULL DEFAULT 'NGN',
  total_quantity     int NOT NULL CHECK (total_quantity >= 0),
  available_quantity int NOT NULL CHECK (available_quantity >= 0),
  max_per_order      int NOT NULL DEFAULT 10 CHECK (max_per_order > 0),
  sales_start        timestamptz NOT NULL,
  sales_end          timestamptz NOT NULL,
  CHECK (available_quantity <= total_quantity)
);

CREATE TABLE orders (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      uuid NOT NULL REFERENCES users(id),
  event_id     uuid NOT NULL REFERENCES events(id),
  status       text NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending','paid','fulfilled','expired','cancelled','refunded')),
  total_kobo   bigint NOT NULL CHECK (total_kobo >= 0),
  currency     char(3) NOT NULL DEFAULT 'NGN',
  expires_at   timestamptz NOT NULL,   -- reservation deadline while pending
  paid_at      timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_pending_expiry_idx ON orders (expires_at) WHERE status = 'pending';

CREATE TABLE order_items (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id       uuid NOT NULL REFERENCES orders(id),
  ticket_type_id uuid NOT NULL REFERENCES ticket_types(id),
  quantity       int  NOT NULL CHECK (quantity > 0),
  unit_price_kobo bigint NOT NULL CHECK (unit_price_kobo >= 0)
);

CREATE TABLE payments (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id     uuid NOT NULL REFERENCES orders(id),
  provider     text NOT NULL DEFAULT 'paystack',
  reference    text NOT NULL UNIQUE,
  status       text NOT NULL DEFAULT 'initiated'
               CHECK (status IN ('initiated','succeeded','failed','refund_pending','refunded')),
  amount_kobo  bigint NOT NULL,
  currency     char(3) NOT NULL,
  provider_data jsonb,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_events (
  id          bigserial PRIMARY KEY,
  provider    text NOT NULL,
  dedupe_key  text NOT NULL,           -- e.g. event type + transaction reference/id
  event_type  text NOT NULL,
  payload     jsonb NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz,
  error       text,
  UNIQUE (provider, dedupe_key)
);

CREATE TABLE tickets (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id       uuid NOT NULL REFERENCES orders(id),
  order_item_id  uuid NOT NULL REFERENCES order_items(id),
  seq            int  NOT NULL,        -- 1..quantity within the order item
  ticket_type_id uuid NOT NULL REFERENCES ticket_types(id),
  event_id       uuid NOT NULL REFERENCES events(id),
  owner_id       uuid NOT NULL REFERENCES users(id),
  code           text NOT NULL UNIQUE, -- random, unguessable token encoded in the QR
  status         text NOT NULL DEFAULT 'valid'
                 CHECK (status IN ('valid','checked_in','cancelled','refunded')),
  issued_at      timestamptz NOT NULL DEFAULT now(),
  checked_in_at  timestamptz,
  checked_in_by  uuid REFERENCES users(id),
  UNIQUE (order_item_id, seq)          -- makes issuance idempotent
);

CREATE TABLE check_in_attempts (
  id         bigserial PRIMARY KEY,
  ticket_id  uuid REFERENCES tickets(id),
  attendant_id uuid NOT NULL REFERENCES users(id),
  result     text NOT NULL,            -- accepted | already_used | invalid | wrong_event
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE idempotency_keys (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         uuid NOT NULL REFERENCES users(id),
  scope           text NOT NULL,       -- e.g. 'create_order', 'init_payment'
  key             text NOT NULL,
  request_hash    text NOT NULL,       -- same key + different body => 422
  response_status int,
  response_body   jsonb,
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, scope, key)
);

CREATE TABLE outbox_events (
  id             bigserial PRIMARY KEY,
  aggregate_type text NOT NULL,
  aggregate_id   uuid NOT NULL,
  event_type     text NOT NULL,        -- e.g. order.paid, ticket.issued
  payload        jsonb NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  published_at   timestamptz,
  attempts       int NOT NULL DEFAULT 0
);
CREATE INDEX outbox_unpublished_idx ON outbox_events (id) WHERE published_at IS NULL;

CREATE TABLE audit_logs (
  id          bigserial PRIMARY KEY,
  actor_id    uuid REFERENCES users(id),
  action      text NOT NULL,
  entity_type text NOT NULL,
  entity_id   text NOT NULL,
  metadata    jsonb,
  ip          inet,
  created_at  timestamptz NOT NULL DEFAULT now()
);
```

(Plus a `sessions` table storing hashed session tokens with `user_id`, `expires_at`, and revocation.)

### State machines

Order, payment, and ticket status are **separate** and never used interchangeably.

```
Order:   pending ──► paid ──► fulfilled
            │          └────► refunded
            ├──► expired
            └──► cancelled

Payment: initiated ──► succeeded ──► refund_pending ──► refunded
                  └──► failed

Ticket:  valid ──► checked_in
            ├──► cancelled
            └──► refunded
```

Every transition is performed with a conditional update (`WHERE status = <expected>`) so concurrent actors cannot skip or repeat a step.

---

## Purchase lifecycle

1. **Select**: client sends event, ticket type(s), quantity, and an `Idempotency-Key`.
2. **Reserve + create order** (one transaction): atomically decrement `available_quantity`, insert a `pending` order with `expires_at = now() + 10 min`, insert order items.
3. **Initialize payment**: create a `payments` row with a unique reference, call Paystack to get the checkout URL.
4. **Customer pays** on Paystack's hosted page.
5. **Webhook arrives**: verify signature on the **raw body**, insert into `webhook_events` (duplicate → acknowledge and stop).
6. **Verify independently**: call Paystack's verify endpoint; check status, amount (kobo), currency, and reference against our records. The browser redirect is *never* proof of payment.
7. **Complete order** (one transaction): mark payment `succeeded`, order `paid`, insert `order.paid` into the outbox.
8. **Fulfil asynchronously**: worker issues tickets (idempotent), generates QR codes, sends the confirmation email, marks the order `fulfilled`.

**Abandoned or expired orders:** a sweeper claims expired `pending` orders (`FOR UPDATE SKIP LOCKED`), marks them `expired`, and returns stock to `available_quantity`.

**Payment succeeds after expiry:** reconcile explicitly. If stock is still available, re-reserve and fulfil. If not, mark the payment `refund_pending` and issue a refund via Paystack. This rule is written down and tested, not left to chance.

---

## Key design decisions

### Inventory: atomic conditional update

```sql
UPDATE ticket_types
SET    available_quantity = available_quantity - $2
WHERE  id = $1
  AND  available_quantity >= $2
  AND  now() BETWEEN sales_start AND sales_end
RETURNING price_kobo, currency;
```

Zero rows returned means sold out or not on sale. The check and the decrement are a single atomic statement, and the `CHECK` constraints are a safety net. When an order spans several ticket types, lock them in a **consistent order** (sorted by id) to avoid deadlocks, and retry on serialization/deadlock errors. `SELECT ... FOR UPDATE` followed by an `UPDATE` in one transaction is also correct; the conditional update is just simpler and holds locks for less time.

### The pending order *is* the reservation

Stock held for a pending order lives in the order and its items, released when the order expires. This avoids a second table whose state could drift from the order's state.

### Idempotency

Mutating endpoints (create order, initiate payment) require an `Idempotency-Key`. The key is stored with a hash of the request. Same key and same request returns the stored response; same key with a different body is rejected. Uniqueness is enforced by the database, not by application checks.

### Webhooks

- Verify the `x-paystack-signature` header (HMAC-SHA512 of the raw body with the secret key) before parsing anything.
- Persist first, process second. Respond `200` quickly; heavy work runs asynchronously.
- Dedupe on `(provider, dedupe_key)`; fulfilment is idempotent anyway via `UNIQUE (order_item_id, seq)`.

### Transactional outbox

State change and the event describing it are committed in the same transaction. A relay polls unpublished rows with `FOR UPDATE SKIP LOCKED`, dispatches them to job handlers, and marks them published. Delivery is **at-least-once**, so every handler is idempotent.

### Jobs and retries

Exponential backoff with jitter, a maximum attempt count, and a dead-letter state for jobs that keep failing, with an alert when the dead-letter count is non-zero.

### Redis is optional and subordinate

Start with Postgres-backed jobs. Add Redis (and Asynq) when measured load or operational needs justify it. Redis never decides ticket availability.

---

## API overview

All endpoints are under `/api/v1`. Full spec in `api/openapi.yaml`.

| Method | Path | Role | Notes |
|--------|------|------|-------|
| POST | `/auth/register` | public | rate limited |
| POST | `/auth/login` | public | rate limited, sets session cookie |
| POST | `/auth/logout` | any | revokes session |
| GET | `/events` | public | published events, paginated |
| GET | `/events/{id}` | public | includes ticket types |
| POST | `/events` | organizer | create draft |
| PATCH | `/events/{id}` | organizer | owner only |
| POST | `/events/{id}/publish` | organizer | validates ticket types exist |
| POST | `/events/{id}/ticket-types` | organizer | |
| POST | `/orders` | customer | requires `Idempotency-Key` |
| GET | `/orders/{id}` | customer | owner only |
| POST | `/orders/{id}/pay` | customer | requires `Idempotency-Key` |
| POST | `/webhooks/paystack` | Paystack | signature verified |
| GET | `/tickets` | customer | my tickets |
| GET | `/tickets/{id}/qr` | customer | owner only |
| POST | `/check-in` | gate attendant | body: ticket code |
| GET | `/events/{id}/report` | organizer | sales and attendance |
| GET | `/healthz` / `/readyz` | internal | liveness / readiness |
| GET | `/metrics` | internal | Prometheus |

Errors use a consistent JSON shape with a stable machine-readable `code`.

---

## Getting started

### Prerequisites

- Go (current stable)
- Docker and Docker Compose
- `make`
- [Goose](https://github.com/pressly/goose), [sqlc](https://sqlc.dev), [k6](https://k6.io) (optional for load tests)
- A Paystack account (test keys) and a tunnel such as ngrok or cloudflared for receiving webhooks locally

### Run locally

```bash
git clone https://github.com/<you>/ticketing-platform.git
cd ticketing-platform

cp .env.example .env          # fill in Paystack test keys and secrets
make up                       # starts PostgreSQL (and Redis if enabled) via docker compose
make migrate-up               # applies Goose migrations
make sqlc                     # generates type-safe query code
make run-api                  # terminal 1
make run-worker               # terminal 2
```

Expose your local API to Paystack and set the webhook URL in the Paystack dashboard:

```bash
ngrok http 8080
# webhook URL: https://<id>.ngrok.app/api/v1/webhooks/paystack
```

### Useful make targets

| Target | What it does |
|--------|--------------|
| `make up` / `make down` | Start / stop local dependencies |
| `make migrate-up` / `migrate-down` | Apply / roll back migrations |
| `make sqlc` | Regenerate query code |
| `make test` | Unit tests with the race detector |
| `make test-integration` | Integration tests using Testcontainers |
| `make test-concurrency` | Oversell and duplicate-webhook tests |
| `make load-test` | k6 purchase-flow scenario |
| `make lint` | golangci-lint |

---

## Configuration

Configuration comes from environment variables (12-factor). Secrets are never committed.

| Variable | Description |
|----------|-------------|
| `APP_ENV` | `development` / `staging` / `production` |
| `HTTP_ADDR` | API listen address, e.g. `:8080` |
| `DATABASE_URL` | PostgreSQL connection string |
| `DB_MAX_CONNS` | Connection pool size |
| `REDIS_URL` | Redis address (only if Asynq is enabled) |
| `SESSION_SECRET` | Secret for signing session/CSRF tokens |
| `PAYSTACK_SECRET_KEY` | Paystack secret key |
| `PAYSTACK_CALLBACK_URL` | Where customers land after payment |
| `RESERVATION_TTL` | Reservation lifetime, default `10m` |
| `EMAIL_PROVIDER_API_KEY` | Email provider credentials |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Tracing collector endpoint |
| `SENTRY_DSN` | Error monitoring |

---

## Testing strategy

Testing starts in Phase 1, not at the end.

| Type | What it covers |
|------|----------------|
| Unit | Pricing, validation, state transitions |
| Integration | SQL queries and transactions against a real PostgreSQL (Testcontainers) |
| API | Handlers, auth, RBAC, error shapes |
| **Concurrency** | N goroutines/clients buying a limited inventory |
| **Payment** | Duplicate, reordered, forged, and wrong-amount webhooks |
| Load | k6 simulating a ticket-sale spike |
| Security | Authorization bypass, injection, abuse of rate limits |
| End-to-end | Register → browse → reserve → pay (test mode) → receive ticket → check in |

**Headline test:** 10 tickets available, 100 concurrent purchase attempts. Expected: exactly 10 succeed, 90 are rejected, `available_quantity = 0`, never negative.

Also required:

- Same webhook delivered 5 times, in parallel → exactly one set of tickets.
- Same `Idempotency-Key` sent twice → one order.
- Worker killed mid-job → job retried, no duplicate tickets.
- Reservation expires while payment is in flight → reconciliation path taken.

Run `go test -race ./...` in CI on every push.

---

## Observability

- **Logs:** structured JSON via `slog`, with request ID, user ID, order ID, and trace ID on every line. Never log secrets, card data, or full webhook bodies containing PII.
- **Metrics (Prometheus):** orders created, orders by status, payment failures, webhook processing time and failures, reservation latency, outbox backlog (oldest unpublished event age), dead-lettered jobs, DB pool saturation, HTTP latency and error rates.
- **Tracing (OpenTelemetry):** API → DB → Paystack → worker spans linked across the outbox.
- **Health:** `/healthz` (process is alive) and `/readyz` (DB reachable, migrations current).
- **Alerts to configure first:** payment failure spike, outbox backlog age, dead-letter count > 0, 5xx rate, DB connection exhaustion.

Dashboards should answer: *How many orders are being created? How many payments are failing? How many jobs are stuck? How long does reservation take? Are there any duplicate ticket issuances (there should be zero)?*

---

## Security

- Argon2id password hashing; session tokens stored hashed server-side.
- HTTP-only, `Secure`, `SameSite` cookies; CSRF protection for cookie-authenticated mutations.
- RBAC enforced in middleware **and** re-checked at the resource level (an organizer can only touch their own events).
- Parameterized queries only (sqlc/pgx); strict input validation; request size limits; CORS allow-list.
- Rate limiting on auth, order creation, and check-in endpoints.
- Webhook signature verification on the raw body; constant-time comparison.
- Ticket codes are random and unguessable; check-in validates event and status atomically.
- Secrets from environment or a secrets manager; audit logs for payments and admin actions.

---

## Deployment and CI/CD

**CI (GitHub Actions)** on every pull request: lint, `go vet`, `go test -race`, integration tests (Testcontainers), `sqlc` diff check, migration up/down check, Docker build, vulnerability scan (`govulncheck`).

**CD:** build immutable images for `api` and `worker`, run migrations as a separate pre-deploy step, deploy, then smoke-test `/readyz`.

**Runtime:**

- Run `api` and `worker` as separate containers/processes; scale them independently.
- Reverse proxy (Caddy or Nginx) terminates TLS.
- Graceful shutdown: stop accepting requests, finish in-flight work, release claimed jobs.
- Automated PostgreSQL backups with a **tested restore**.
- Migrations are backwards compatible (expand, then contract) so deploys do not need downtime.

---

## Operations runbook

| Symptom | Likely cause | Action |
|---------|--------------|--------|
| Customer paid but has no tickets | Webhook missed or fulfilment job failed | Check `webhook_events` and `outbox_events` for the order; run the reconciliation job, which verifies the payment with Paystack and fulfils |
| Outbox backlog growing | Worker down or handler failing | Check worker health and logs; failing events surface in the dead-letter view |
| Inventory looks too low | Expired orders not swept | Check sweeper job; confirm `orders_pending_expiry_idx` is used |
| Duplicate ticket suspected | Should be impossible | Query on `(order_item_id, seq)`; treat as a P1 and inspect the unique constraint |
| Webhooks rejected | Wrong secret or body altered by a proxy | Confirm the raw body is used for signature verification |

A scheduled **reconciliation job** compares recent Paystack transactions against local payments and flags mismatches.

---

## Build roadmap

Each phase ends with a demonstrable exit criterion. Do not start the next phase until it is met.

- [ ] **Phase 0: Decisions and skeleton.** Write short ADRs (modular monolith, Postgres as source of truth, money in kobo, pending order = reservation). Repo, Makefile, docker compose, CI running lint and tests.
- [ ] **Phase 1: Database and foundation.** Schema and Goose migrations, sqlc wiring, config, logging, health endpoints, graceful shutdown. *Exit: migrations run up and down cleanly in CI.*
- [ ] **Phase 2: Auth and RBAC.** Registration, login, sessions, role middleware, rate limiting. *Exit: authorization tests prove each role can and cannot do what it should.*
- [ ] **Phase 3: Events and ticket types.** CRUD, publish rules, ownership checks, pagination. *Exit: API tests pass; organizers cannot touch others' events.*
- [ ] **Phase 4: Inventory and orders.** Atomic reservation, pending orders, expiry sweeper, idempotency keys. *Exit: the 10-tickets / 100-buyers test passes with `-race`.*
- [ ] **Phase 5: Payments.** Paystack initialization, webhook endpoint, signature verification, independent verification, late-payment reconciliation. *Exit: duplicate, forged, and wrong-amount webhooks tested.*
- [ ] **Phase 6: Outbox, workers, fulfilment.** Outbox relay, idempotent ticket issuance, QR generation, emails, retries, dead-letter handling. *Exit: killing a worker mid-job never duplicates or loses tickets.*
- [ ] **Phase 7: Check-in and reporting.** Atomic check-in, attempt log, organizer reports.
- [ ] **Phase 8: Observability and hardening.** Metrics, tracing, dashboards, alerts, security review, audit logs.
- [ ] **Phase 9: Load and failure testing.** k6 sale-spike scenario, kill-the-API and kill-the-worker drills, restore-from-backup drill.
- [ ] **Phase 10: Production deployment.** HTTPS, CD pipeline, monitoring live, runbook verified. Introduce Redis/Asynq only if measurements justify it.

---

## Out of scope (for now)

Seat maps and assigned seating, waiting lists, dynamic pricing, multi-currency, secondary-market transfers, SMS notifications, CQRS read models, and microservices. Each can be added later without changing the core guarantees above.

---

## License

MIT (or your preferred license). See `LICENSE`.
