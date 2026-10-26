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

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           payprovider.NewServer(pool, payprovider.NewPayments(pool, cfg.SlowCardDelay), logger).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

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
