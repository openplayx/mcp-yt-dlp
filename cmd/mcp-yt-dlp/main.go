// Command mcp-yt-dlp is the OpenPlayX MCP service wrapping yt-dlp.
//
//	mcp-yt-dlp --config config.json --secrets secrets.json [--host H] [--port P]
//	mcp-yt-dlp version
//	mcp-yt-dlp healthcheck URL    exit 0 when URL answers 200 (Docker HEALTHCHECK)
//
// Environment: YTDLP_MCP_CONFIG / YTDLP_MCP_SECRETS (defaults for the flags),
// YTDLP_BIN (yt-dlp executable, default "yt-dlp" on PATH).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/openplayx/mcp-yt-dlp/internal/auth"
	"github.com/openplayx/mcp-yt-dlp/internal/config"
	"github.com/openplayx/mcp-yt-dlp/internal/engine"
	"github.com/openplayx/mcp-yt-dlp/internal/jobs"
	"github.com/openplayx/mcp-yt-dlp/internal/netguard"
	"github.com/openplayx/mcp-yt-dlp/internal/server"
	"github.com/openplayx/mcp-yt-dlp/internal/service"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println(version)
			return
		case "healthcheck":
			os.Exit(healthcheck(os.Args[2:]))
		}
	}
	configPath := flag.String("config", os.Getenv("YTDLP_MCP_CONFIG"), "config.json path")
	secretsPath := flag.String("secrets", os.Getenv("YTDLP_MCP_SECRETS"), "secrets.json path")
	host := flag.String("host", "", "overrides server.host")
	port := flag.Int("port", 0, "overrides server.port")
	flag.Parse()
	if err := run(*configPath, *secretsPath, *host, *port); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func logLevel(name string) slog.Level {
	switch name {
	case "DEBUG":
		return slog.LevelDebug
	case "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func run(configPath, secretsPath, host string, port int) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	secrets, err := config.LoadSecrets(secretsPath)
	if err != nil {
		return err
	}
	proxy, err := config.ResolveProxy(cfg, secrets)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel(cfg.Server.LogLevel)}))
	slog.SetDefault(log)
	if err := os.MkdirAll(cfg.Storage.CacheDir, 0o755); err != nil {
		return err
	}
	bin := os.Getenv("YTDLP_BIN")
	if bin == "" {
		bin = "yt-dlp"
	}
	eng := engine.NewYtDlp(bin, cfg, proxy, log)
	store, err := jobs.OpenStore(cfg.Storage.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	jm, err := jobs.NewManager(cfg, eng, store, jobs.SystemDiskFree, log)
	if err != nil {
		return err
	}
	svc := service.New(cfg, eng, jm, netguard.SystemResolver, version)
	if len(secrets.Tokens) == 0 {
		log.Warn("no API tokens configured; every /mcp request is rejected")
	}
	if host == "" {
		host = cfg.Server.Host
	}
	if port == 0 {
		port = cfg.Server.Port
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(svc, auth.New(secrets, cfg.Limits.RequestsPerMinute), log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "version", version, "yt_dlp", eng.Version())
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	jm.Shutdown(10 * time.Second)
	return nil
}

func healthcheck(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mcp-yt-dlp healthcheck URL")
		return 2
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
