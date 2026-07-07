package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/aigateway/cli"
	"github.com/aigateway/config"
	"github.com/aigateway/server"
)

func main() {
	// Optimize Go runtime for low-spec VPS (1 vCPU / 1GB RAM)
	// GOGC=50 triggers GC more aggressively to keep memory low
	// GOMEMLIMIT=750MiB caps total Go heap to prevent OOM
	if os.Getenv("GOGC") == "" {
		os.Setenv("GOGC", "50")
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		os.Setenv("GOMEMLIMIT", "750MiB")
	}
	debug.SetGCPercent(50)

	if len(os.Args) < 2 {
		// Default mode: start server + terminal config simultaneously
		runServerWithCLI("config.yaml")
		return
	}

	switch os.Args[1] {
	case "config", "cfg":
		// Config-only mode: no server, just terminal CLI
		cfgPath := "config.yaml"
		if len(os.Args) > 2 {
			cfgPath = os.Args[2]
		}
		cli.Run(cfgPath)

	case "serve", "start":
		// Server-only mode: no terminal CLI
		cfgPath := "config.yaml"
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		configPath := fs.String("config", cfgPath, "Path to configuration file")
		fs.Parse(os.Args[2:])
		runServer(*configPath)

	case "help", "--help", "-h":
		printUsage()

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

// runServerWithCLI starts the server in background and runs the terminal config CLI in foreground.
// This allows configuring via both web dashboard and terminal simultaneously.
func runServerWithCLI(configPath string) {
	// Load config first
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		fmt.Printf("Failed to load configuration: %v\nRun 'aigateway config' to create one interactively.\n", err)
		os.Exit(1)
	}

	// Setup logging to file (so it doesn't corrupt the TUI)
	logFile, err := os.OpenFile("aigateway.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("Failed to open log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	var level slog.Level
	switch strings.ToLower(cfg.Server.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger := slog.New(&CustomLogHandler{w: logFile, level: level})
	slog.SetDefault(logger)

	// Disable tunnel output directly to stdout/stderr in CLI mode
	server.DisableStdoutTunnelPrint = true

	// Start server in background goroutine
	// IMPORTANT: Use os.Stderr for banner so it doesn't corrupt bubbletea's stdout control
	go func() {
		srv, err := server.New(cfg, configPath)
		if err != nil {
			slog.Error("failed to initialize server", "error", err)
			fmt.Fprintf(os.Stderr, "Server init error: %v (check aigateway.log)\n", err)
			return
		}
		hostDisplay := cfg.Server.Host
		if hostDisplay == "0.0.0.0" {
			hostDisplay = "localhost" // lebih bersahabat untuk diklik langsung di terminal lokal
		}
		addr := fmt.Sprintf("%s:%d", hostDisplay, cfg.Server.Port)
		fmt.Fprintf(os.Stderr, "\n  🚀 Server running at http://%s\n", addr)
		fmt.Fprintf(os.Stderr, "  📊 Dashboard: http://%s/admin\n", addr)
		fmt.Fprintf(os.Stderr, "  🎮 Playground: http://%s/playground\n", addr)
		if cfg.Server.Host == "0.0.0.0" {
			fmt.Fprintf(os.Stderr, "  🌐 Remote Access: http://[vps-ip-atau-domain]:%d\n", cfg.Server.Port)
		}
		fmt.Fprintf(os.Stderr, "  📝 Logs: aigateway.log\n\n")
		if err := srv.Start(); err != nil {
			slog.Error("server stopped", "error", err)
		}
	}()

	// Small delay so server prints its banner first
	time.Sleep(500 * time.Millisecond)
	// Then run terminal CLI on main thread
	cli.Run(configPath)
}

// runServer starts the server only (blocking).
func runServer(configPath string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		fmt.Printf("failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	var level slog.Level
	switch strings.ToLower(cfg.Server.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	logger := slog.New(&CustomLogHandler{w: os.Stdout, level: level})
	slog.SetDefault(logger)
	slog.Info("configuration loaded", "path", configPath, "log_level", cfg.Server.LogLevel)

	srv, err := server.New(cfg, configPath)
	if err != nil {
		slog.Error("failed to initialize server", "error", err)
		os.Exit(1)
	}

	if err := srv.Start(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`AI Gateway — OpenAI-compatible multi-provider proxy

Usage:
  aigateway                  Start server + terminal config (recommended)
  aigateway config [path]    Interactive config CLI only (no server)
  aigateway serve [flags]    Start server only (no terminal)
  aigateway help             Show this help

Flags:
  -config string   Path to configuration file (default "config.yaml")

Modes:
  aigateway                    Server runs in background, terminal config in foreground
                               You can configure via web (http://localhost:8080/admin)
                               AND terminal at the same time!
  aigateway config             Terminal config only, no server
  aigateway serve              Server only, no terminal (logs to stdout)`)
}

// CustomLogHandler formats log entries into clean, readable lines.
type CustomLogHandler struct {
	w     io.Writer
	level slog.Level
}

func (h *CustomLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *CustomLogHandler) Handle(ctx context.Context, r slog.Record) error {
	timeStr := r.Time.Format("15:04:05")
	msg := r.Message

	// Format level badge for standard logs
	levelStr := ""
	switch r.Level {
	case slog.LevelDebug:
		levelStr = "[DEBUG] "
	case slog.LevelWarn:
		levelStr = "[WARN] "
	case slog.LevelError:
		levelStr = "[ERROR] "
	}

	// Catch custom pretty logs starting with emojis or PENDING status
	isPretty := strings.HasPrefix(msg, "📥") ||
		strings.HasPrefix(msg, "ℹ️") ||
		strings.HasPrefix(msg, "📊") ||
		strings.HasPrefix(msg, "🌊") ||
		strings.HasPrefix(msg, "❌") ||
		strings.HasPrefix(msg, "[PENDING]")

	if isPretty {
		fmt.Fprintf(h.w, "[%s] %s\n", timeStr, msg)
	} else {
		// Standard log line
		attrs := ""
		r.Attrs(func(a slog.Attr) bool {
			attrs += fmt.Sprintf(" | %s=%v", a.Key, a.Value)
			return true
		})
		fmt.Fprintf(h.w, "[%s] %s%s%s\n", timeStr, levelStr, msg, attrs)
	}
	return nil
}

func (h *CustomLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *CustomLogHandler) WithGroup(name string) slog.Handler {
	return h
}
