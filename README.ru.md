# Cloute

Облачный текстовый редактор для совместной работы небольших команд и учебных групп. Несколько человек правят один документ одновременно и видят правки и курсоры друг друга; одновременные правки сливаются автоматически через CRDT (RGA, написан вручную на Go и TypeScript), в том числе после работы офлайн. Сервис разворачивается на собственном сервере организации.

- Совместная правка в реальном времени, курсоры и выделения участников.
- Офлайн-режим: правки копятся локально и сливаются после переподключения без потерь.
- Роли `owner` / `editor` / `commenter` / `viewer`, доступ по ссылке с ролью.
- Комментарии к фрагментам текста, не «съезжающие» при правках.
- История версий и откат, который не теряет одновременные правки других.

Протокол клиент–сервер: [api/protocol.md](api/protocol.md). HTTP API: [api/http.md](api/http.md).

## Разработка

Нужны Go (версия из `go.mod`), Node.js 20.19+ или 22.12+ (требование Vite), Docker.

```bash
make db-up                 # PostgreSQL в Docker на 127.0.0.1:5434
npm --prefix web ci        # один раз
make run                   # сервер на :8080 (миграции применяются при старте)
npm --prefix web run dev   # клиент на http://localhost:5173 (проксирует /api и /ws на :8080)
```

Проверки:

```bash
gofmt -l .                 # пусто
go vet ./...
go test -race ./...        # нужен make db-up; с -short тесты с БД пропускаются
make lint                  # gofmt, go vet, golangci-lint
npm --prefix web run build && npm --prefix web test
```

`make build` собирает один исполняемый файл `bin/cloute` с встроенным фронтендом (`go build -tags embedweb`). Без тега `embedweb` фронтенд не встраивается — так `go build ./...` работает и до сборки фронтенда.

## Развёртывание

Сервер — один статический бинарник с фронтендом внутри; ему нужен только PostgreSQL 16+. Готовый вариант — Docker Compose из `deploy/`: образ собирается из исходников (`deploy/Dockerfile`, около 26 МБ, distroless, без shell, не от root).

### Запуск

```bash
export CLOUTE_DB_PASSWORD='длинный-случайный-пароль'
export CLOUTE_COOKIE_SECURE=true
docker compose -f deploy/docker-compose.yml --profile app up -d --build --wait
curl -fsS http://127.0.0.1:8080/healthz
```

Без `--profile app` compose поднимает только базу (это и делает `make db-up` для разработки). Миграции схемы применяются сервером при каждом старте; одновременный запуск нескольких экземпляров безопасен — миграции идут под блокировкой в БД.

- **Пароль БД** (`CLOUTE_DB_PASSWORD`) задаётся до первого запуска: PostgreSQL применяет его только при создании тома `pgdata`. Если пароль содержит символы, особые для URL (`@ : / ? #`), закодируйте их — он подставляется в `CLOUTE_DATABASE_URL`.
- **Порт** приложения открыт только на `127.0.0.1` (`CLOUTE_APP_PORT`, по умолчанию 8080): снаружи сервис доступен через обратный прокси с HTTPS.
- **Логи** — JSON в stdout: `docker compose -f deploy/docker-compose.yml logs -f app`.
- **Остановка** (`docker compose ... stop app`) штатная: сервер дописывает снапшоты документов и закрывает соединения клиентов, клиенты переподключаются сами.

### HTTPS и обратный прокси

Сессия хранится в cookie, поэтому в интернете сервис должен работать только по HTTPS, с `CLOUTE_COOKIE_SECURE=true`. Прокси обязан:

- передавать исходный `Host`: по нему сервер проверяет `Origin` у WebSocket и изменяющих запросов (защита от CSRF) — без этого всё будет отклоняться с 403;
- пропускать WebSocket (`Upgrade`) на `/ws` и не обрывать его раньше, чем через минуту тишины (сервер пингует раз в 30 с).

Пример для nginx (адаптируйте под свой сервер):

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
        deny all; # метрики — только для Prometheus из внутренней сети
    }
}
```

### Переменные окружения

| Переменная | По умолчанию | Смысл |
|---|---|---|
| `CLOUTE_DATABASE_URL` | — (обязательна) | `postgres://user:password@host:5432/db?sslmode=…` |
| `CLOUTE_HTTP_ADDR` | `:8080` | адрес HTTP-сервера |
| `CLOUTE_COOKIE_SECURE` | `false` | `true` за HTTPS: cookie сессии только по защищённому соединению |
| `CLOUTE_LOG_FORMAT` | `text` (в образе `json`) | `text` или `json` |
| `CLOUTE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `CLOUTE_SHUTDOWN_TIMEOUT` | `10s` | сколько ждать запросы и закрытие соединений при остановке |

В compose дополнительно: `CLOUTE_DB_PASSWORD`, `CLOUTE_APP_PORT`, `CLOUTE_DB_PORT`.

### Резервное копирование и обновление

Всё состояние — в PostgreSQL (лог операций, снапшоты, версии, комментарии, пользователи):

```bash
docker compose -f deploy/docker-compose.yml exec -T postgres pg_dump -U cloute -Fc cloute > cloute-$(date +%F).dump
```

Обновление — пересборка и перезапуск: `docker compose -f deploy/docker-compose.yml --profile app up -d --build --wait`. Подключённые клиенты на время перезапуска уходят в офлайн-режим и после него досылают свои правки.

## Наблюдаемость

`GET /metrics` — метрики в формате Prometheus (плюс стандартные `go_*` и `process_*`):

| Метрика | Тип | Что показывает |
|---|---|---|
| `cloute_ws_connections` | gauge | открытые WebSocket-соединения |
| `cloute_documents_loaded` | gauge | документы в памяти (без клиентов документ выгружается через минуту) |
| `cloute_ops_total` | counter | принятые и записанные операции; `rate(cloute_ops_total[1m])` — операций в секунду |
| `cloute_broadcast_seconds` | histogram | задержка рассылки: от приёма операций сервером до записи их в сокет каждого другого клиента (включая запись в БД) |

Например, 95-й перцентиль задержки рассылки: `histogram_quantile(0.95, rate(cloute_broadcast_seconds_bucket[5m]))`.

`GET /healthz` отвечает, что процесс жив; базу данных он не проверяет.
