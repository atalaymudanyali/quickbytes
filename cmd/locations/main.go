// Command locations runs the location service: couriers report where they are, and
// it keeps the latest position of each in Redis.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/locations"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("location service stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := locations.LoadConfig()
	if err != nil {
		return err
	}

	// ctx is cancelled on Ctrl+C or when Docker stops the container (SIGTERM).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cache.DiscardLogs() // the service logs Redis failures itself, at most every 10 s
	redisClient, err := cache.Open(cfg.RedisURL)
	if err != nil {
		return err
	}
	defer redisClient.Close()
	// Not fatal: pings are dropped until Redis answers, and then it just works.
	if err := cache.Ping(ctx, redisClient); err != nil {
		logger.Warn("redis not reachable at startup; dropping pings until it is", "err", err)
	}

	service := locations.NewService(cfg.ShiftKeySecret, locations.NewStore(redisClient), logger)

	udpAddr, err := net.ResolveUDPAddr("udp", cfg.UDPAddr)
	if err != nil {
		return fmt.Errorf("udp address: %w", err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("udp door: %w", err)
	}
	udpDone := make(chan struct{})
	go func() {
		defer close(udpDone)
		logger.Info("udp door listening", "addr", cfg.UDPAddr)
		service.ServeUDP(ctx, udpConn)
	}()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           service.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		// Open WebSocket calls end with ctx. Shutdown below doesn't wait for them:
		// once a call has become a WebSocket, the HTTP server lets go of it.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	stop()
	<-udpDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
