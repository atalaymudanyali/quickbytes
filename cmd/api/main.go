// Command api runs the QuickBytes API service.
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

	"github.com/redis/go-redis/v9"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
	"github.com/atalaymudanyali/quickbytes/internal/auth"
	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/carts"
	"github.com/atalaymudanyali/quickbytes/internal/config"
	"github.com/atalaymudanyali/quickbytes/internal/database"
	"github.com/atalaymudanyali/quickbytes/internal/httpapi"
	"github.com/atalaymudanyali/quickbytes/internal/idempotency"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
	"github.com/atalaymudanyali/quickbytes/internal/orders"
	"github.com/atalaymudanyali/quickbytes/internal/payments"
	"github.com/atalaymudanyali/quickbytes/internal/ratelimit"
	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
	"github.com/atalaymudanyali/quickbytes/internal/users"
	"github.com/atalaymudanyali/quickbytes/migrations"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
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

	if err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		return err
	}

	// Redis is optional and "fails open": if it's missing or down, the API still
	// works, reading menus from the database and logging in without rate limits.
	// So a failed ping is a warning, not a reason to stop.
	cache.UseLogger(logger)
	var redisClient *redis.Client
	if cfg.RedisURL == "" {
		logger.Warn("REDIS_URL not set: running without a cache or rate limits")
	} else {
		redisClient, err = cache.Open(cfg.RedisURL)
		if err != nil {
			return err
		}
		defer redisClient.Close()
		if err := cache.Ping(ctx, redisClient); err != nil {
			logger.Warn("redis not reachable at startup; reading from the database until it is", "err", err)
		}
	}

	menuStore := menus.NewStore(pool)
	menuCache := menus.NewCache(menuStore, redisClient, menus.DefaultTTL, logger)
	restaurantStore := restaurants.NewStore(pool)
	addressStore := addresses.NewStore(pool)
	// The cart prices from the same menu cache that serves public menus.
	cartStore := carts.NewStore(pool, menuCache, restaurantStore)
	orderStore := orders.NewStore(pool, cartStore, addressStore)
	idempotencyStore := idempotency.NewStore(pool)
	// The one plug in the payments socket today: our mock provider over HTTP.
	provider := payments.NewPayProvider(cfg.PayProviderURL, cfg.PayProviderKey, cfg.PayProviderTimeout)
	paymentStore := payments.NewStore(pool, provider, logger)
	// Every order move notes, in the same commit, that the order's payment may
	// need work (capture, void, refund).
	orderStore.OnMove(paymentStore.MarkDueIn)

	tokens, err := auth.NewTokenIssuer(cfg.JWTSecret, cfg.AccessTokenTTL)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewServer(httpapi.Deps{
			DB:          pool,
			Users:       users.NewStore(pool),
			Hasher:      auth.NewHasher(auth.DefaultParams),
			Tokens:      tokens,
			Refresh:     auth.NewRefreshStore(pool, cfg.RefreshTokenTTL),
			Restaurants: restaurantStore,
			Menus:       menuStore,
			Addresses:   addressStore,
			Carts:       cartStore,
			Orders:      orderStore,
			Payments:    paymentStore,
			Idempotency: idempotencyStore,
			MenuCache:   menuCache,
			Limiter:     ratelimit.New(redisClient, logger),
			Logger:      logger,
		}).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Unpaid orders are cancelled after the pay window. The loop stops with ctx,
	// on shutdown.
	go orderStore.RunExpiry(ctx, cfg.OrderExpiryEvery, cfg.OrderPayWindow, logger)
	// Payments that need work (an unknown result, a slow card, a money step
	// that didn't go through) are settled with the provider. Stops with ctx.
	go paymentStore.RunReconcile(ctx, cfg.PaymentReconcileEvery, logger)
	// Idempotency keys older than 24 hours are deleted. This loop stops with ctx too.
	go idempotencyStore.RunCleanup(ctx, cfg.IdempotencyCleanupEvery, logger)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.HTTPAddr)
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
