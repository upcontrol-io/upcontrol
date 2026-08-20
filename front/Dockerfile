# The app as a container, so `docker compose up` is the whole product and not
# "the backend, plus run npm yourself". Two stages: build the static bundle with
# node, serve it with Caddy — the runtime image carries no node and no sources.
#
# Local development still uses `npm run dev` for hot reload; this image is what
# the self-host compose runs behind its Caddy edge.
# --platform=$BUILDPLATFORM: the bundle (tsc + vite) is architecture-independent
# static output, so the build stage always runs natively on the builder instead
# of under QEMU for the arm64 half of a multi-arch build. Only the caddy stage
# below is per-architecture.
FROM --platform=$BUILDPLATFORM node:22-alpine AS build
WORKDIR /src
# Manifests first: the dependency layer is cached until they actually change.
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
# Type errors fail the image, not the browser.
RUN npm run prod

FROM caddy:2-alpine
COPY --from=build /src/dist /srv
COPY docker/Caddyfile /etc/caddy/Caddyfile
EXPOSE 80
