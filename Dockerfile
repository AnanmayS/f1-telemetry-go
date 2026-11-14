# syntax=docker/dockerfile:1

FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ingest ./cmd/ingest

# Distroless: no shell, no package manager, runs as an unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /out/ingest /usr/local/bin/
COPY --from=web /src/web/dist /srv/web
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
# -trust-proxy is not a default: it is only safe when every request arrives
# through Cloudflare. deploy/docker-compose.yml adds it.
CMD ["-addr", ":8080", "-data", "/data", "-static", "/srv/web"]
