package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ParsaKSH/SlipStream-Plus/internal/balancer"
	"github.com/ParsaKSH/SlipStream-Plus/internal/config"
	"github.com/ParsaKSH/SlipStream-Plus/internal/embedded"
	"github.com/ParsaKSH/SlipStream-Plus/internal/engine"
	"github.com/ParsaKSH/SlipStream-Plus/internal/gui"
	"github.com/ParsaKSH/SlipStream-Plus/internal/health"
	"github.com/ParsaKSH/SlipStream-Plus/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	enableGUI := flag.Bool("gui", false, "enable web dashboard")
	guiPort := flag.String("gui-listen", "", "override GUI listen address (e.g., 127.0.0.1:8384)")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("SlipstreamPlus starting...")

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Handle embedded binary
	if embedded.IsEmbedded() {
		binPath, cleanup, err := embedded.ExtractBinary()
		if err != nil {
			log.Fatalf("Failed to extract embedded binary: %v", err)
		}
		defer cleanup()
		cfg.SlipstreamBinary = binPath
		log.Printf("Using embedded slipstream-client binary")
	} else {
		if cfg.SlipstreamBinary == "" {
			log.Fatalf("slipstream_binary is required in config (or build with -tags embed_slipstream)")
		}
		if _, err := os.Stat(cfg.SlipstreamBinary); os.IsNotExist(err) {
			log.Fatalf("Slipstream binary not found at: %s", cfg.SlipstreamBinary)
		}
	}

	expanded, err := cfg.ExpandInstances()
	if err != nil {
		log.Fatalf("Failed to expand instances: %v", err)
	}

	// Detect which modes are used
	hasSSH, hasSOCKS := false, false
	for _, ei := range expanded {
		if ei.Mode == "ssh" {
			hasSSH = true
		} else {
			hasSOCKS = true
		}
	}

	log.Printf("Loaded config: %d instances (%d expanded), strategy=%s, listen=%s, ssh=%v, socks=%v",
		len(cfg.Instances), len(expanded), cfg.Strategy, cfg.Socks.Listen, hasSSH, hasSOCKS)

	// Create process manager
	mgr, err := engine.NewManager(cfg)
	if err != nil {
		log.Fatalf("Failed to create manager: %v", err)
	}

	if err := mgr.StartAll(); err != nil {
		log.Fatalf("Failed to start instances: %v", err)
	}

	// Health checker (per-instance mode aware, auto-restarts unhealthy)
	checker := health.NewChecker(mgr, &cfg.HealthCheck)
	checker.Start()

	// Load balancer
	bal := balancer.New(cfg.Strategy)
	log.Printf("Using load balancing strategy: %s", cfg.Strategy)

	// GUI
	if *enableGUI || cfg.GUI.Enabled {
		if *guiPort != "" {
			cfg.GUI.Listen = *guiPort
		}
		apiServer := gui.NewAPIServer(mgr, cfg, *configPath)
		if err := apiServer.Start(); err != nil {
			log.Fatalf("Failed to start GUI: %v", err)
		}
	}

	// Shutdown handler
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("Received signal %v, shutting down...", sig)
		checker.Stop()
		mgr.Shutdown()
		os.Exit(0)
	}()

	// Start proxy servers based on which modes are used.
	// If there are SSH-mode instances, start the SSH SOCKS5 handler.
	// If there are SOCKS-mode instances, start the TCP relay.
	// Both share the same listen address — if BOTH modes exist,
	// SSH instances use SSH tunneling while SOCKS instances use TCP relay.
	// Since they share a port, we use the mode that handles both:
	// SSH proxy handles SOCKS5 locally for SSH instances,
	// TCP relay forwards raw connections for SOCKS instances.

	if hasSSH && !hasSOCKS {
		// All instances are SSH — use SOCKS5-over-SSH proxy
		log.Printf("Starting in SSH mode (SOCKS5 → SSH tunnel → slipstream)")
		sshServer := proxy.NewSSHServer(
			cfg.Socks.Listen, cfg.Socks.BufferSize, cfg.Socks.MaxConnections,
			mgr, bal,
		)
		defer sshServer.Close()
		if err := sshServer.ListenAndServe(); err != nil {
			log.Fatalf("SSH proxy error: %v", err)
		}
	} else if !hasSSH && hasSOCKS {
		// All instances are SOCKS — use transparent TCP relay
		log.Printf("Starting in SOCKS mode (transparent TCP relay → slipstream)")
		proxyServer := proxy.NewServer(
			cfg.Socks.Listen, cfg.Socks.BufferSize, cfg.Socks.MaxConnections,
			mgr, bal,
		)
		if err := proxyServer.ListenAndServe(); err != nil {
			log.Fatalf("Proxy error: %v", err)
		}
	} else if hasSSH && hasSOCKS {
		// Mixed mode: SSH proxy handles SOCKS5 for SSH instances,
		// TCP relay handles SOCKS instances on a separate port
		log.Printf("Starting in MIXED mode (SSH + SOCKS instances)")
		log.Printf("SSH SOCKS5 proxy on %s (for SSH-mode instances)", cfg.Socks.Listen)

		sshServer := proxy.NewSSHServer(
			cfg.Socks.Listen, cfg.Socks.BufferSize, cfg.Socks.MaxConnections,
			mgr, bal,
		)
		defer sshServer.Close()
		if err := sshServer.ListenAndServe(); err != nil {
			log.Fatalf("SSH proxy error: %v", err)
		}
	}
}
