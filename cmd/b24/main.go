package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/mcp"
	"github.com/SeRGei93/b24-mcp/internal/server"
	"github.com/SeRGei93/b24-mcp/internal/store"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func main() {
	if len(os.Args) > 1 && isHealthcheckArg(os.Args[1]) {
		if err := runHealthcheck(); err != nil {
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		logger := config.StderrLogger("error")
		logger.Error("Fatal startup error", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
}

func isHealthcheckArg(arg string) bool {
	switch strings.TrimSpace(arg) {
	case "-healthcheck", "--healthcheck", "healthcheck":
		return true
	default:
		return false
	}
}

func runHealthcheck() error {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "3000"
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected health status: %d", resp.StatusCode)
	}
	return nil
}

func run() error {
	cfg, err := config.LoadFromOS()
	if err != nil {
		return err
	}
	logger := config.StderrLogger(cfg.LogLevel)

	st, err := store.NewStore(cfg.DatabasePath, cfg.TokenEncryptionKey)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := st.Migrate(ctx, logger); err != nil {
		return err
	}

	if _, err := st.EnsureWebhookConnection(ctx, cfg); err != nil {
		return err
	}

	oauth := bitrix.NewOAuthService(cfg, st, nil)
	tokens := bitrix.NewTokenManager(st, oauth, logger)
	rc := bitrix.NewRestClient(cfg, tokens, logger, oauth.HTTPClient())
	schema := bitrix.NewSchemaDiscovery(cfg, st, rc)
	crm := bitrix.NewCrmService(cfg, rc)
	ops := bitrix.NewOperationService(cfg, st, rc)

	if cfg.Transport == "stdio" {
		mcpSrv := mcp.NewServer(st, schema, crm, ops, "")
		return mcpserver.ServeStdio(mcpSrv)
	}

	mcpOAuth := mcp.NewOAuthService(cfg, st)
	handler := server.NewHandlerWithServices(cfg, st, oauth, tokens, mcpOAuth, schema, crm, ops, logger)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("Server listening", map[string]any{"address": addr})
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("Shutting down", nil)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}
