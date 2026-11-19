# syntax=docker/dockerfile:1

FROM node:25-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
COPY testdata/ /src/testdata/
RUN npm run build

FROM golang:1.27-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY migrations/ migrations/
COPY web/*.go web/
COPY --from=web /src/web/dist web/dist
RUN CGO_ENABLED=0 go build -tags embedweb -trimpath -ldflags="-s -w" -o /cloute ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=server /cloute /cloute
ENV CLOUTE_HTTP_ADDR=:8080 \
    CLOUTE_LOG_FORMAT=json
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/cloute"]
