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
	"runtime"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/JudiLite/CDT-Monitor/internal/aliyun"
	"github.com/JudiLite/CDT-Monitor/internal/engine"
	"github.com/JudiLite/CDT-Monitor/internal/httpapi"
	"github.com/JudiLite/CDT-Monitor/internal/notify"
	"github.com/JudiLite/CDT-Monitor/internal/store"
	"github.com/JudiLite/CDT-Monitor/internal/web"
)

var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

func main() {
	command := "serve"
	if len(os.Args) > 1 && os.Args[1][0] != '-' {
		command = os.Args[1]
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
	}
	dataDir := flag.String("data", envOr("CDT_DATA_DIR", "./data"), "persistent data directory")
	listen := flag.String("listen", envOr("CDT_LISTEN", ":8080"), "HTTP listen address")
	workers := flag.Int("workers", envInt("CDT_WORKERS", 4), "background worker count")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if command == "version" {
		fmt.Printf("cdt-monitor %s (%s, %s, %s/%s)\n", version, commit, builtAt, runtime.GOOS, runtime.GOARCH)
		return
	}
	if command == "healthcheck" {
		client := &http.Client{Timeout: 3 * time.Second}
		response, checkErr := client.Get("http://127.0.0.1" + normalizeListen(*listen) + "/healthz")
		if checkErr != nil || response.StatusCode != http.StatusOK {
			if response != nil {
				response.Body.Close()
			}
			os.Exit(1)
		}
		response.Body.Close()
		return
	}
	st, err := store.Open(*dataDir)
	if err != nil {
		logger.Error("open data store", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	if command == "migrate" {
		logger.Info("database migrations complete", "data_dir", *dataDir)
		return
	}

	provider := aliyun.NewClient()
	notifier := notify.New()
	eng := engine.New(st, provider, notifier, logger, *workers)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	eng.Start(ctx)

	if command == "run-once" {
		if err = eng.RunOnce(ctx); err != nil {
			logger.Error("run monitor cycle", "error", err)
			os.Exit(1)
		}
		deadline := time.Now().Add(70 * time.Second)
		for time.Now().Before(deadline) {
			count, countErr := st.CountQueuedJobs(ctx)
			if countErr != nil || count == 0 {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		logger.Info("monitor cycle complete")
		return
	}
	if command != "serve" {
		logger.Error("unknown command", "command", command)
		os.Exit(2)
	}

	api := httpapi.New(st, eng, web.FS(), logger, httpapi.BuildInfo{Version: version, Commit: commit, BuiltAt: builtAt})
	server := &http.Server{
		Addr: *listen, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	go func() {
		logger.Info("CDT Monitor started", "listen", *listen, "version", version, "data_dir", *dataDir)
		if serveErr := server.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", serveErr)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	logger.Info("CDT Monitor stopped")
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func envInt(key string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(key)); err == nil && value > 0 {
		return value
	}
	return fallback
}

func normalizeListen(listen string) string {
	if len(listen) > 0 && listen[0] == ':' {
		return listen
	}
	if _, port, err := net.SplitHostPort(listen); err == nil {
		return ":" + port
	}
	return ":8080"
}
