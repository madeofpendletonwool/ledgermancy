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

# The document vault's storage root, created here and owned by the user the
# process actually runs as.
#
# This is not decoration. Docker creates a named volume owned by root:root, and
# this container runs as uid 10001 — so mounting a volume at a path that does
# not exist in the image gives the api a directory it cannot write to, and the
# vault disables itself at startup. When the mount point *does* exist in the
# image, Docker initialises an empty volume from it and carries this ownership
# across, which is what makes the default compose deploy work unattended.
#
# An operator who already has a root-owned volume from an earlier build has to
# discard it once: `docker compose down && docker volume rm ledgermancy_documents-data`.
RUN mkdir -p /var/lib/ledgermancy/documents \
 && chown -R ledgermancy:ledgermancy /var/lib/ledgermancy \
 && chmod 700 /var/lib/ledgermancy/documents

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
