// Command payprovider runs the mock payment provider: a fake card processor
// that behaves like an outside company (Stripe, iyzico), with its own database.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/database"
	"github.com/atalaymudanyali/quickbytes/internal/idempotency"
	"github.com/atalaymudanyali/quickbytes/internal/payprovider"
	providermigrations "github.com/atalaymudanyali/quickbytes/migrations/payprovider"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("payment provider stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := payprovider.LoadConfig()
	if err != nil {
		return err
	}

	// ctx is cancelled on Ctrl+C or when Docker stops the container (SIGTERM).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The provider's own migrations, on its own database.
	if err := database.Migrate(ctx, pool, providermigrations.FS); err != nil {
		return err
	}

	merchant, err := payprovider.NewMerchants(pool).Ensure(ctx, cfg.MerchantName, cfg.MerchantKey)
	if err != nil {
		return err
	}
	logger.Info("merchant ready", "merchant_id", merchant.ID, "name", merchant.Name)

	// The merchant's webhook endpoint, from the environment like its key.
	endpoints := payprovider.NewEndpoints(pool)
	if cfg.WebhookURL != "" {
		ep, err := endpoints.Ensure(ctx, merchant.ID, cfg.WebhookURL, cfg.WebhookSecret)
		if err != nil {
			return err
		}
		logger.Info("webhooks on", "endpoint_id", ep.ID, "url", ep.URL)
	} else {
		if err := endpoints.Disable(ctx, merchant.ID); err != nil {
			return err
		}
		logger.Info("webhooks off (no WEBHOOK_URL)")
	}

	payments := payprovider.NewPayments(pool, cfg.SlowCardDelay)
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           payprovider.NewServer(pool, payments, logger).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: the hang card's answer takes 30 seconds on purpose.
	}

	// Slow-card payments get their final answer when their time comes. All
	// the loops stop with ctx, on shutdown.
	go payments.RunSettler(ctx, cfg.SettleEvery, logger)
	// Queued webhooks are posted, and tried again later when they fail.
	sender := payprovider.NewSender(pool, payprovider.SenderConfig{
		Timeout: cfg.WebhookTimeout, SendTwice: cfg.WebhookSendTwice,
	}, logger)
	go sender.RunSender(ctx, cfg.WebhookSendEvery)
	if cfg.WebhookSendTwice {
		logger.Warn("WEBHOOK_SEND_TWICE is on: every webhook is posted twice")
	}
	// Idempotency keys older than 24 hours are deleted, as in the API.
	go idempotency.NewStore(pool).RunCleanup(ctx, cfg.IdempotencyCleanupEvery, logger)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("payment provider listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Graceful shutdown: stop accepting new requests, let in-flight ones finish.
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
