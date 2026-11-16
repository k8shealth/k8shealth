# Same logic as Java agent: version passed at build time (Maven -Dagent.version=IMAGE_TAG).
# Usage: make build [VERSION=1.25]   or   VERSION=1.25 make build
VERSION ?= 0.0.1-SNAPSHOT
LDFLAGS := -ldflags "-X github.com/fx/health-agent-go/config.Version=$(VERSION)"

.PHONY: build build-linux stamp-agent-sh
build:
	go build $(LDFLAGS) -o health-agent-go .

# Optional: stamp ../health-agent/agent.sh before publishing script (content hash; unchanged on Go-only rebuilds).
stamp-agent-sh:
	cd ../health-agent && chmod +x stamp-script-version.sh && ./stamp-script-version.sh

# Linux binary for Docker (same as Dockerfile does)
build-linux:
	CGO_ENABLED=0 GOOS=linux go build $(LDFLAGS) -o health-agent-go .

clean:
	rm -f health-agent-go health-agent-go.exe
