// Package main provides the entry point for Kiro API Proxy.
//
// Kiro API Proxy is a reverse proxy service that translates Kiro API requests
// into OpenAI and Anthropic (Claude) compatible formats. Key features include:
//   - Multi-account pool with round-robin load balancing
//   - Automatic OAuth token refresh
//   - Streaming response support for real-time AI interactions
//   - Admin panel for account and configuration management
//
// The service exposes the following endpoints:
//   - /v1/messages - Claude API compatible endpoint
//   - /v1/chat/completions - OpenAI API compatible endpoint
//   - /admin - Web-based administration panel
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"kiro-go/proxy"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const shutdownTimeout = 30 * time.Second

func main() {
	// CLI flags are runtime-only overrides. Preserve zsec/main precedence:
	// flag > environment (PORT/HOST) > persisted config.
	portFlag := flag.Int("port", -1, "HTTP listen port (overrides PORT env and config.json)")
	hostFlag := flag.String("host", "", "HTTP bind host (overrides HOST env and config.json)")
	flag.Parse()

	if err := runWithOverrides(*hostFlag, *portFlag); err != nil {
		log.Printf("Kiro-Go stopped with error: %v", err)
		os.Exit(1)
	}
}

// run keeps the lifecycle entry point usable by tests/embedders without CLI
// overrides while still honoring the PORT/HOST environment variables.
func run() error {
	return runWithOverrides("", -1)
}

func runWithOverrides(hostOverride string, portOverride int) error {
	// 配置文件路径，支持环境变量覆盖
	configPath := "data/config.json"
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}

	// 确保数据目录存在
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	// 加载配置
	if err := config.Init(configPath); err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Initialize log level: LOG_LEVEL env var takes priority over config, defaulting to "info".
	logger.Init(config.GetLogLevel())

	// 环境变量覆盖密码
	if envPassword := os.Getenv("ADMIN_PASSWORD"); envPassword != "" {
		config.SetPassword(envPassword)
	}

	// Runtime listen-address overrides must be resolved before net.Listen below.
	// Environment overrides persisted config; explicit CLI flags override env.
	if envPort := os.Getenv("PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil && p > 0 && p <= 65535 {
			config.SetPort(p)
		} else {
			logger.Warnf("Ignoring invalid PORT env value %q", envPort)
		}
	}
	if envHost := os.Getenv("HOST"); envHost != "" {
		config.SetHost(envHost)
	}
	if portOverride >= 0 {
		if portOverride == 0 || portOverride > 65535 {
			return fmt.Errorf("invalid -port %d: must be 1-65535", portOverride)
		}
		config.SetPort(portOverride)
	}
	if hostOverride != "" {
		config.SetHost(hostOverride)
	}

	// Register process signals before binding or starting any background work.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// Bind before creating the pool/handler so a port conflict starts no workers.
	addr := fmt.Sprintf("%s:%d", config.GetHost(), config.GetPort())
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	defer listener.Close()
	if signalCtx.Err() != nil {
		stopSignals()
		return nil
	}

	// The application root owns process-global pool shutdown.
	accountPool := pool.GetPool()
	// 创建 HTTP 处理器（包含后台刷新任务）
	handler := proxy.NewHandler()
	tracker := newRequestTracker()
	requestRoot, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()

	logger.Infof("Kiro-Go starting on http://%s (log level: %s)", addr, logger.LevelName(logger.GetLevel()))
	logger.Infof("Admin panel: http://%s/admin", addr)
	logger.Infof("Claude API: http://%s/v1/messages", addr)
	logger.Infof("OpenAI API: http://%s/v1/chat/completions", addr)

	// WriteTimeout intentionally 0: SSE streams can run for minutes while the
	// upstream model produces tokens. ReadHeaderTimeout + ReadTimeout still
	// guard against slowloris-style header/body stalls.
	srv := &http.Server{
		Addr:    addr,
		Handler: tracker.wrap(handler),
		BaseContext: func(net.Listener) context.Context {
			return requestRoot
		},
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	app := &application{
		server:         srv,
		handler:        handler,
		pool:           accountPool,
		tracker:        tracker,
		cancelRequests: cancelRequests,
	}

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- srv.Serve(listener)
	}()

	var serveErr error
	serveResultReceived := false
	select {
	case <-signalCtx.Done():
		logger.Infof("Shutdown signal received")
	case serveErr = <-serveErrCh:
		serveResultReceived = true
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Infof("HTTP server stopped unexpectedly: %v", serveErr)
		}
	}

	// Restore default signal behavior immediately so a second Ctrl+C/SIGTERM can
	// force process exit if graceful shutdown becomes stuck.
	stopSignals()

	var shutdownErrors []error
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		shutdownErrors = append(shutdownErrors, fmt.Errorf("serve HTTP: %w", serveErr))
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	if err := app.shutdown(shutdownCtx); err != nil {
		shutdownErrors = append(shutdownErrors, err)
	}

	if !serveResultReceived {
		select {
		case serveErr = <-serveErrCh:
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("serve HTTP: %w", serveErr))
			}
		case <-shutdownCtx.Done():
			// Do not exceed the single application shutdown deadline just to wait
			// for the Serve goroutine to publish its buffered result.
		}
	}
	cancelShutdown()

	return errors.Join(shutdownErrors...)
}
