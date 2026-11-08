package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/bitrix"
	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/mcp"
	"github.com/SeRGei93/b24-mcp/internal/server"
	"github.com/SeRGei93/b24-mcp/internal/store"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func main() {
	if err := run(); err != nil {
		logger := config.StderrLogger("error")
		logger.Error("Fatal startup error", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
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
	guard := bitrix.NewPolicyGuard()
	ops := bitrix.NewOperationService(cfg, st, guard, rc)

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
