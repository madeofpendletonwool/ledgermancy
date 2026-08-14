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

# One-off maintenance commands, shipped in the api image so an operator can run
# them with `docker compose exec` against the deployment's own DATABASE_URL
# rather than needing a Go toolchain on the host. Both default to a dry run.
RUN CGO_ENABLED=0 go build -trimpath -o /out/normalise-merchant-keys ./cmd/normalise-merchant-keys

# The post-sync transfer pairer only looks back 60 days, so a household's
# existing history stays unlinked until this is run once after deploying.
RUN CGO_ENABLED=0 go build -trimpath -o /out/backfill-transfer-pairs ./cmd/backfill-transfer-pairs

# ---- api -------------------------------------------------------------------
FROM alpine:3.24 AS api
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
COPY --from=build /out/normalise-merchant-keys /usr/local/bin/normalise-merchant-keys
COPY --from=build /out/backfill-transfer-pairs /usr/local/bin/backfill-transfer-pairs
USER ledgermancy
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/api"]

# ---- worker ----------------------------------------------------------------
FROM alpine:3.24 AS worker

# postgresql17-client supplies pg_dump, pg_restore and psql for the scheduled
# backup and the restore test.
#
# The major version is pinned deliberately, and it must track the `postgres:`
# image in docker-compose.yml. pg_dump from an older major cannot read a newer
# server's catalogs, and the failure is the worst kind available here: it can
# produce an archive that looks complete and will not restore. The app checks
# this at run time too (continuity.Dumper.CheckVersions refuses to write a dump
# on a mismatch), but pinning it here is what stops the mismatch existing.
#
# Upgrading Postgres therefore means changing two lines, not one — the compose
# image and this package.
RUN apk add --no-cache ca-certificates tzdata postgresql17-client \
 && adduser -D -u 10001 ledgermancy

# The backup destination, created here and owned by the user the process runs
# as — for the same Docker reason the api stage creates the document root: a
# named volume mounted at a path that does not exist in the image arrives owned
# by root, and the worker runs as uid 10001. With the mount point present,
# Docker initialises the volume from it and carries this ownership across.
#
# 0700 because this directory holds the whole database in restorable form.
RUN mkdir -p /var/lib/ledgermancy/backups \
 && chown -R ledgermancy:ledgermancy /var/lib/ledgermancy \
 && chmod 700 /var/lib/ledgermancy/backups

COPY --from=build /out/worker /usr/local/bin/worker
USER ledgermancy
ENTRYPOINT ["/usr/local/bin/worker"]
