BIN     := bin/cloute
COMPOSE := docker compose -f deploy/docker-compose.yml

# Local database from deploy/docker-compose.yml; set the variable in the
# environment to use another one.
CLOUTE_DATABASE_URL ?= postgres://cloute:cloute@localhost:5434/cloute?sslmode=disable
export CLOUTE_DATABASE_URL

.PHONY: run test lint build web-build db-up db-down

run:
	go run ./cmd/server

test:
	go test -race ./...

lint:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt: unformatted files:"; echo "$$out"; exit 1; fi
	go vet ./...
	golangci-lint run ./...

# One binary with the frontend embedded (needs `npm --prefix web ci` once).
build: web-build
	go build -tags embedweb -trimpath -o $(BIN) ./cmd/server

web-build:
	npm --prefix web run build

db-up:
	$(COMPOSE) up -d --wait

db-down:
	$(COMPOSE) down
