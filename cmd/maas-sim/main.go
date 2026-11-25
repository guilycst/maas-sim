package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"

	"github.com/guilycst/maas-sim/internal/api"
	"github.com/guilycst/maas-sim/internal/driver"
	"github.com/guilycst/maas-sim/internal/seed"
	"github.com/guilycst/maas-sim/internal/store"
)

type config struct {
	listen           string
	port             int
	dnsPort          int
	machines         int
	seedPath         string
	mode             string
	delay            time.Duration
	cleanup          bool
	docker           driver.DockerOptions
	dockerSubnet     string
	explicitMachines bool
}

func main() {
	cfg := config{docker: driver.DockerOptions{Tmpfs: make(map[string]string)}}
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1", "bind address for HTTP and DNS")
	flag.IntVar(&cfg.port, "port", 5240, "MAAS HTTP port")
	flag.IntVar(&cfg.dnsPort, "dns-port", 1053, "authoritative DNS port (0 disables DNS)")
	flag.IntVar(&cfg.machines, "machines", 6, "number of machines to seed")
	flag.StringVar(&cfg.seedPath, "seed", "", "load initial inventory from a YAML or JSON file")
	flag.StringVar(&cfg.docker.Image, "node-image", "", "Docker image for deployed machines")
	flag.StringVar(&cfg.docker.NetworkName, "docker-network", "maas-sim", "Docker bridge network")
	flag.StringVar(&cfg.dockerSubnet, "docker-subnet", "", "IPv4 CIDR for the Docker bridge")
	flag.BoolVar(&cfg.docker.Privileged, "node-privileged", false, "run deployed containers in privileged mode")
	flag.StringVar(&cfg.docker.CgroupNamespace, "node-cgroupns", "", "cgroup namespace (host or private)")
	flag.Func("node-bind", "bind mount for deployed containers (repeatable)", func(value string) error {
		if value == "" {
			return errors.New("bind mount is required")
		}
		cfg.docker.Binds = append(cfg.docker.Binds, value)
		return nil
	})
	flag.Func("node-tmpfs", "tmpfs mount path with optional :options (repeatable)", func(value string) error {
		target, options, _ := strings.Cut(value, ":")
		if !path.IsAbs(target) {
			return errors.New("tmpfs target must be an absolute path")
		}
		cfg.docker.Tmpfs[target] = options
		return nil
	})
	flag.StringVar(&cfg.mode, "mode", "memory", "machine backend (docker or memory)")
	flag.DurationVar(&cfg.delay, "deploy-delay", 200*time.Millisecond, "deployment delay in memory mode")
	flag.BoolVar(&cfg.cleanup, "cleanup", true, "remove simulator-owned Docker resources at shutdown")
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "machines":
			cfg.explicitMachines = true
		case "seed":
			if cfg.seedPath == "" {
				slog.Error("maas-sim", "error", "seed path is required")
				os.Exit(2)
			}
		}
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		slog.Error("maas-sim", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config) (runErr error) {
	if cfg.port < 0 || cfg.port > 65535 || cfg.dnsPort < 0 || cfg.dnsPort > 65535 {
		return errors.New("port must be between 0 and 65535")
	}
	if cfg.delay < 0 {
		return errors.New("deploy-delay must be nonnegative")
	}
	inventory, err := loadInventory(cfg)
	if err != nil {
		return err
	}

	var backend driver.MachineDriver
	switch cfg.mode {
	case "memory":
		backend = driver.NewMemory(cfg.delay)
	case "docker":
		if cfg.dockerSubnet != "" {
			prefix, err := netip.ParsePrefix(cfg.dockerSubnet)
			if err != nil {
				return fmt.Errorf("invalid docker subnet: %w", err)
			}
			cfg.docker.Subnet = prefix
		}
		setupCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		docker, err := driver.NewDocker(setupCtx, cfg.docker)
		cancel()
		if err != nil {
			return err
		}
		defer func() { runErr = errors.Join(runErr, docker.Close()) }()
		backend = docker
	default:
		return fmt.Errorf("unknown mode %q", cfg.mode)
	}
	if cfg.cleanup {
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			runErr = errors.Join(runErr, backend.Cleanup(cleanupCtx))
		}()
	}

	workers, cancel := context.WithCancel(ctx)
	defer cancel()
	handler := api.New(workers, store.New(inventory, backend.Network()), backend)
	return serve(workers, cfg, handler, cancel)
}

func loadInventory(cfg config) (seed.Inventory, error) {
	if cfg.seedPath != "" {
		if cfg.explicitMachines {
			return seed.Inventory{}, errors.New("--seed and --machines cannot be combined")
		}
		return seed.Load(cfg.seedPath)
	}
	if cfg.machines < 1 || cfg.machines > 10000 {
		return seed.Inventory{}, errors.New("machines must be between 1 and 10000")
	}
	return seed.Default(cfg.machines), nil
}

func serve(ctx context.Context, cfg config, handler *api.Server, cancel context.CancelFunc) error {
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.listen, strconv.Itoa(cfg.port)))
	if err != nil {
		return err
	}
	defer listener.Close()

	httpServer := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute,
	}
	failures := make(chan error, 3)
	var dnsUDP, dnsTCP *dns.Server
	if cfg.dnsPort != 0 {
		address := net.JoinHostPort(cfg.listen, strconv.Itoa(cfg.dnsPort))
		packet, err := net.ListenPacket("udp", address)
		if err != nil {
			return err
		}
		defer packet.Close()
		stream, err := net.Listen("tcp", address)
		if err != nil {
			return err
		}
		defer stream.Close()

		dnsUDP = &dns.Server{PacketConn: packet, Handler: handler.DNSHandler()}
		dnsTCP = &dns.Server{Listener: stream, Handler: handler.DNSHandler()}
		go func() { failures <- dnsUDP.ActivateAndServe() }()
		go func() { failures <- dnsTCP.ActivateAndServe() }()
	}

	go func() { failures <- httpServer.Serve(listener) }()
	slog.Info("maas-sim listening", "http", listener.Addr(), "dns_port", cfg.dnsPort, "mode", cfg.mode)
	select {
	case <-ctx.Done():
	case err = <-failures:
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), time.Minute)
	defer stop()
	if dnsUDP != nil {
		_ = dnsUDP.ShutdownContext(shutdown)
		_ = dnsTCP.ShutdownContext(shutdown)
	}
	shutdownErr := httpServer.Shutdown(shutdown)
	if shutdownErr != nil {
		_ = httpServer.Close()
	}
	handler.Wait()
	return errors.Join(err, shutdownErr)
}
