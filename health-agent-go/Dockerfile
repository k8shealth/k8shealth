# Build stage — pass VERSION at build time: docker build --build-arg VERSION=<git-tag> ...
# Jenkins: buildArgs "--build-arg VERSION=${env.IMAGE_TAG}" in jenkinsfile-prod-build.groovy
FROM golang:1.26-alpine AS builder
ARG VERSION=0.0.1-SNAPSHOT
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# -s -w: strip debug info and DWARF to reduce binary size and RSS
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w -X github.com/fx/health-agent-go/config.Version=${VERSION}" -o /health-agent-go .

# Run stage (same port and env as Java agent for side-by-side / Kyverno)
# NOTE: do not bump the Alpine version without updating the Kyverno health-agent policy. Its init
# container copies a hard-coded list of curl shared libraries (libcurl, libz, libnghttp2, libidn2,
# libpsl, libssl, libcrypto, libbrotli, libunistring) into app containers. Alpine 3.20 curl also needs
# libcares and libzstd, so a newer base breaks the in-pod health-agent.sh script.
FROM alpine:3.18
# tini: minimal init process used as a subreaper (tini -s) by health-agent.sh
# to reap BusyBox 'timeout' zombie grandchildren — prevents slab memory leak
# when the app container's PID 1 (Java) does not call wait().
RUN apk --no-cache add ca-certificates curl tini
# Run as an unprivileged user. The agent only needs to read the ServiceAccount namespace file,
# write to /tmp and listen on 8082. The Kyverno init container copies curl/tini into the shared
# emptyDir (world-writable) and chmods only the files it created, so root is not required.
RUN addgroup -S -g 10001 agent && adduser -S -u 10001 -G agent -H -s /sbin/nologin agent
WORKDIR /app
COPY --from=builder /health-agent-go .
USER 10001:10001
EXPOSE 8082
ENV SERVER_PORT=8082
ENV POD_NAME=localhost
ENV DEPLOYMENT=
ENV NAMESPACE=default
ENV MANAGER_URL=http://health-manager.infrastructure.svc.cluster.local:8080
ENV HC_CHECKS_JSON=
# GOGC=50: trigger GC earlier to keep heap lower (default 100); helps stay under 75Mi limit
ENV GOGC=50
CMD ["./health-agent-go"]
