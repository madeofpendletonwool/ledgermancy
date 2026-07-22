# Multi-stage: build the Vite bundle, then serve it from nginx. The same image
# also reverse-proxies /api, /webhooks, and /healthz to the api container over
# the compose network, so the api needs no host port of its own.

# --- build ------------------------------------------------------------------
FROM node:22-alpine AS build
WORKDIR /app

# Install deps against the lockfile first so this layer caches across source
# changes. npm ci needs devDependencies (tsc, vite) — do not set production.
COPY package.json package-lock.json ./
RUN npm ci

COPY . .
RUN npm run build   # tsc -b && vite build -> /app/dist

# --- serve ------------------------------------------------------------------
FROM nginx:1.27-alpine AS serve
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html

# Generates /etc/nginx/trusted-proxies.conf from $TRUSTED_PROXIES on every
# start. The placeholder below is what nginx.conf includes when the entrypoint
# has not run -- an `include` of a missing file is a fatal config error, and
# someone overriding the entrypoint should get a working server, not a crash.
COPY docker-entrypoint.d/25-trusted-proxies.sh /docker-entrypoint.d/
RUN chmod +x /docker-entrypoint.d/25-trusted-proxies.sh \
    && echo '# Replaced at container start; see 25-trusted-proxies.sh.' \
        > /etc/nginx/trusted-proxies.conf

EXPOSE 80
