# syntax=docker/dockerfile:1

# ---- build -----------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first so the module cache survives source-only edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/api  ./cmd/api
RUN CGO_ENABLED=0 go build -trimpath -o /out/worker ./cmd/worker

# ---- api -------------------------------------------------------------------
FROM alpine:3.21 AS api
RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -u 10001 ledgermancy
COPY --from=build /out/api /usr/local/bin/api
USER ledgermancy
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/api"]

# ---- worker ----------------------------------------------------------------
FROM alpine:3.21 AS worker
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 ledgermancy
COPY --from=build /out/worker /usr/local/bin/worker
USER ledgermancy
ENTRYPOINT ["/usr/local/bin/worker"]
