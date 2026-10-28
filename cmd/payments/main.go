package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"example.com/payment-integrity/internal/service"
	"github.com/jackc/pgx/v5/pgxpool"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func main() {
	if err := run(); err != nil {
		slog.Error("payments stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if os.Getenv("DATABASE_URL") == "" {
		return errors.New("DATABASE_URL is required")
	}
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	s := &service.Store{DB: db, Version: env("STRIPE_API_VERSION", service.APIVersion)}
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "migrate":
		return s.Migrate(ctx)
	case "reconcile", "reconcile-fixture":
		jobCtx, stop := context.WithTimeout(ctx, 5*time.Minute)
		defer stop()
		var provider service.Provider = service.Stripe{Key: os.Getenv("STRIPE_SECRET_KEY"), Version: s.Version}
		if command == "reconcile-fixture" {
			if len(os.Args) != 3 {
				return errors.New("usage: payments reconcile-fixture PATH.json")
			}
			data, readErr := os.ReadFile(os.Args[2])
			if readErr != nil {
				return readErr
			}
			var snapshot service.Snapshot
			if err := json.Unmarshal(data, &snapshot); err != nil {
				return err
			}
			provider = service.FixtureProvider{Data: snapshot}
		}
		report, err := s.Reconcile(jobCtx, provider, time.Now().UTC())
		if err != nil {
			return err
		}
		if err = json.NewEncoder(os.Stdout).Encode(report); err != nil {
			return err
		}
		if len(report.Issues) > 0 {
			return fmt.Errorf("reconciliation found %d discrepancies", len(report.Issues))
		}
		return nil
	case "serve":
		if len(os.Getenv("ADMIN_TOKEN")) < 16 || os.Getenv("STRIPE_WEBHOOK_SECRET") == "" {
			return errors.New("STRIPE_WEBHOOK_SECRET and ADMIN_TOKEN (16+ characters) are required")
		}
		workers, err := strconv.Atoi(env("WORKERS", "4"))
		if err != nil || workers < 1 || workers > 64 {
			return errors.New("WORKERS must be between 1 and 64")
		}
		server := &http.Server{Addr: env("ADDR", ":8080"), Handler: service.API{Store: s, WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"), AdminToken: os.Getenv("ADMIN_TOKEN")}.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); s.Work(ctx) }()
		}
		errs := make(chan error, 1)
		go func() { errs <- server.ListenAndServe() }()
		select {
		case <-ctx.Done():
		case err = <-errs:
			if !errors.Is(err, http.ErrServerClosed) {
				cancel()
				wg.Wait()
				return err
			}
		}
		cancel()
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		err = server.Shutdown(shutdown)
		wg.Wait()
		return err
	default:
		return errors.New("usage: payments [migrate|serve|reconcile|reconcile-fixture PATH.json]")
	}
}
