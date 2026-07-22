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
EXPOSE 80
