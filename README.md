# Cloute

A cloud text editor for small teams and study groups to work on together.
Several people edit one document at the same time and see each other's edits
and cursors; concurrent edits merge automatically through a CRDT (RGA, written
by hand in Go and TypeScript), including after working offline. The service is
deployed on the organisation's own server.

- Real-time collaborative editing, participants' cursors and selections.
- Offline mode: edits pile up locally and merge after reconnecting, nothing is lost.
- Roles `owner` / `editor` / `commenter` / `viewer`, access by link with a role.
- Comments on text ranges that do not drift when the text is edited.
- Version history and a rollback that does not lose other people's concurrent edits.

Client–server protocol: [api/protocol.md](api/protocol.md). HTTP API:
[api/http.md](api/http.md) (both in Russian).

## Development

You need Go (the version from `go.mod`), Node.js 20.19+ or 22.12+ (a Vite
requirement) and Docker.

```bash
make db-up                 # PostgreSQL in Docker on 127.0.0.1:5434
npm --prefix web ci        # once
make run                   # server on :8080 (migrations are applied at startup)
npm --prefix web run dev   # client on http://localhost:5173 (proxies /api and /ws to :8080)
```

Checks:

```bash
gofmt -l .                 # empty
go vet ./...
go test -race ./...        # needs make db-up; with -short the database tests are skipped
make lint                  # gofmt, go vet, golangci-lint
npm --prefix web run build && npm --prefix web test
```

`make build` produces a single executable `bin/cloute` with the frontend
embedded (`go build -tags embedweb`). Without the `embedweb` tag the frontend is
not embedded — that way `go build ./...` works even before the frontend is
built.

## Deployment

The server is one static binary with the frontend inside; all it needs is
PostgreSQL 16+. A ready-made option is Docker Compose from `deploy/`: the image
is built from source (`deploy/Dockerfile`, about 26 MB, distroless, no shell,
not running as root).

### Running

```bash
export CLOUTE_DB_PASSWORD='a-long-random-password'
export CLOUTE_COOKIE_SECURE=true
docker compose -f deploy/docker-compose.yml --profile app up -d --build --wait
curl -fsS http://127.0.0.1:8080/healthz
```

Without `--profile app` compose starts only the database (which is exactly what
`make db-up` does for development). Schema migrations are applied by the server
on every start; running several instances at once is safe — migrations run
under a lock in the database.

- **Database password** (`CLOUTE_DB_PASSWORD`) must be set before the first
  run: PostgreSQL applies it only when the `pgdata` volume is created. If the
  password contains characters that are special in URLs (`@ : / ? #`), encode
  them — it is substituted into `CLOUTE_DATABASE_URL`.
- **Port** of the app is bound to `127.0.0.1` only (`CLOUTE_APP_PORT`, 8080 by
  default): from outside the service is reached through a reverse proxy with
  HTTPS.
- **Logs** are JSON on stdout:
  `docker compose -f deploy/docker-compose.yml logs -f app`.
- **Stopping** (`docker compose ... stop app`) is graceful: the server writes
  out document snapshots and closes client connections, and the clients
  reconnect on their own.

### HTTPS and a reverse proxy

The session lives in a cookie, so on the internet the service must run over
HTTPS only, with `CLOUTE_COOKIE_SECURE=true`. The proxy must:

- pass the original `Host`: the server checks the `Origin` of WebSocket and
  state-changing requests against it (CSRF protection) — without it everything
  is rejected with 403;
- let WebSocket (`Upgrade`) through on `/ws` and not cut it off before a minute
  of silence (the server pings every 30 s).

An nginx example (adapt it to your server):

```nginx
server {
    listen 443 ssl;
    server_name cloute.example.org;
    # ssl_certificate ...; ssl_certificate_key ...;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto https;
    }

    location /ws {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 120s;
    }

    location /metrics {
        deny all; # metrics are for Prometheus on the internal network only
    }
}
```

### Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `CLOUTE_DATABASE_URL` | — (required) | `postgres://user:password@host:5432/db?sslmode=…` |
| `CLOUTE_HTTP_ADDR` | `:8080` | HTTP server address |
| `CLOUTE_COOKIE_SECURE` | `false` | `true` behind HTTPS: the session cookie is sent over secure connections only |
| `CLOUTE_LOG_FORMAT` | `text` (`json` in the image) | `text` or `json` |
| `CLOUTE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `CLOUTE_SHUTDOWN_TIMEOUT` | `10s` | how long to wait for requests and connections to close on shutdown |

Compose additionally uses `CLOUTE_DB_PASSWORD`, `CLOUTE_APP_PORT` and
`CLOUTE_DB_PORT`.

### Backup and upgrade

All the state is in PostgreSQL (the operation log, snapshots, versions,
comments, users):

```bash
docker compose -f deploy/docker-compose.yml exec -T postgres pg_dump -U cloute -Fc cloute > cloute-$(date +%F).dump
```

Upgrading is a rebuild and restart:
`docker compose -f deploy/docker-compose.yml --profile app up -d --build --wait`.
Connected clients go into offline mode for the duration of the restart and send
their edits afterwards.

## Observability

`GET /metrics` — metrics in Prometheus format (plus the standard `go_*` and
`process_*`):

| Metric | Type | What it shows |
|---|---|---|
| `cloute_ws_connections` | gauge | open WebSocket connections |
| `cloute_documents_loaded` | gauge | documents in memory (a document with no clients is unloaded after a minute) |
| `cloute_ops_total` | counter | operations accepted and stored; `rate(cloute_ops_total[1m])` — operations per second |
| `cloute_broadcast_seconds` | histogram | broadcast latency: from the server receiving operations to writing them to every other client's socket (including the database write) |

For example, the 95th percentile of broadcast latency:
`histogram_quantile(0.95, rate(cloute_broadcast_seconds_bucket[5m]))`.

`GET /healthz` answers that the process is alive; it does not check the
database.

Resource exhaustion forecasts for these metrics:
[foresight](https://github.com/te1pe/foresight).

## Project layout

```
cmd/server/            main: config, logger, database, HTTP server, graceful shutdown
internal/
  crdt/                the CRDT core (RGA), pure Go, no I/O and no dependencies
  doc/                 document Hub: applying operations, broadcasting, presence
  transport/ws/        WebSocket: reading and writing protocol messages
  httpapi/             REST: documents, users, access links, versions
  storage/postgres/    repositories on top of pgx
  auth/                users, sessions, roles
  config/              configuration from environment variables
migrations/            goose SQL migrations
web/                   client (Vite + TypeScript): src/crdt/ — the CRDT port, src/sync/ — the protocol
testdata/crdt/         shared CRDT test vectors (JSON) for Go and TypeScript
api/                   protocol and HTTP API specs
docs/decisions/        architecture decision records
deploy/                docker-compose.yml, Dockerfile
```

The Go and TypeScript CRDT implementations must produce identical results on
the test vectors in `testdata/crdt/`.

Документация на русском: [README.ru.md](README.ru.md).
