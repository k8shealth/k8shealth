// k8shealth health-agent
//
// Sidecar agent that runs in each monitored pod: it executes the configured health
// checks (HTTP, MySQL, file), collects what the pod can see, and reports to a
// k8shealth manager.
//
// Part of k8shealth - Kubernetes health monitoring.  https://k8shealth.io
// Copyright (c) 2026 Myfxbook Ltd

package main

import (
	"log"

	"github.com/fx/health-agent-go/config"
	"github.com/fx/health-agent-go/health"
	"github.com/fx/health-agent-go/registry"
	"github.com/fx/health-agent-go/server"
	"github.com/fx/health-agent-go/sync"
)

func main() {
	cfg := config.Load()
	reg := registry.New(cfg)

	if installAgentSecret(cfg.ManagerURL) {
		config.LogWarnf("manager requests will carry X-Agent-Secret")
	}

	// Credit line at warn+ (same level as the boot line below) so it shows in default mode, once per start
	config.LogWarnf("k8shealth health-agent - https://k8shealth.io")
	// Boot line at warn+ so default mode still shows identity without debug spam
	config.LogWarnf("health-agent starting: namespace=%s pod=%s ip=%s deployment=%s port=%d",
		cfg.Namespace, cfg.PodName, cfg.PodIP, cfg.Deployment, cfg.ServerPort)
	config.LogDebugf("Discovered config (verbose):")
	config.LogDebugf(" - Namespace: %s", cfg.Namespace)
	config.LogDebugf(" - Pod Name: %s", cfg.PodName)
	config.LogDebugf(" - Pod IP: %s", cfg.PodIP)
	config.LogDebugf(" - Deployment: %s", cfg.Deployment)
	config.LogDebugf(" - Port: %d", cfg.ServerPort)

	// HTTP server (same port as Java: 8082)
	go func() {
		srv := &server.Server{Cfg: cfg, Registry: reg}
		if err := srv.Run(); err != nil {
			log.Fatalf("Server: %v", err)
		}
	}()

	// /proc/net/tcp scanner: accumulates 24h union of outbound TCP connections.
	// Runs only when /proc/net/tcp is available (Linux pods). No-op on Windows/local dev.
	connScanner := health.NewConnectionScanner()
	go connScanner.Run()

	// Infrastructure software fingerprint (Apache/nginx/Tomcat/Jetty/HAProxy/MySQL/Redis...) of this pod,
	// rescanned every few hours; the manager flags outdated / end-of-life versions.
	softwareScanner := health.NewSoftwareScanner(cfg.ServerPort)
	connScanner.AttachSoftwareScanner(softwareScanner)
	health.SetDefaultSoftwareScanner(softwareScanner)
	go softwareScanner.Run()

	// Sync intervals from manager settings (same as Java SyncIntervalConfig); shared by manager and agent sync
	intervals := sync.NewIntervals()

	// Manager sync: fetch intervals, callback + inactive keys
	go sync.RunManagerSyncWithScanner(cfg, reg, intervals, connScanner)

	// Agent sync: POST to peers (uses same intervals)
	go sync.RunAgentSync(cfg, reg, intervals)

	// Health checks from HEALTH_AGENT_HC_CHECKS_JSON or HC_CHECKS_JSON
	go health.RunHealthChecks(cfg, reg)

	select {}
}
