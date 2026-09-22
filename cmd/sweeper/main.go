// Command sweeper periodically expires reservations whose deadline has passed and
// returns their units to available stock. It runs until interrupted, or sweeps once
// and exits when SWEEP_ONCE is set.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"stocklock/internal/sweep"
)

const (
	defaultDatabaseURL = "postgres://stocklock:stocklock@localhost:5432/stocklock"
	defaultInterval    = time.Second
	defaultBatch       = 100
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = defaultDatabaseURL
	}
	interval := defaultInterval
	if v := os.Getenv("SWEEP_INTERVAL"); v != "" {
		var err error
		if interval, err = time.ParseDuration(v); err != nil || interval <= 0 {
			log.Fatalf("SWEEP_INTERVAL %q: want a positive duration such as 1s", v)
		}
	}
	batch := defaultBatch
	if v := os.Getenv("SWEEP_BATCH"); v != "" {
		var err error
		if batch, err = strconv.Atoi(v); err != nil || batch <= 0 {
			log.Fatalf("SWEEP_BATCH %q: want a positive integer", v)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatal(err)
	}

	if os.Getenv("SWEEP_ONCE") != "" {
		n, err := sweep.Drain(ctx, pool, batch)
		if err != nil {
			log.Fatalf("sweep: %v", err)
		}
		log.Printf("expired %d reservation(s)", n)
		return
	}

	log.Printf("sweeping every %v, batch %d", interval, batch)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n, err := sweep.Drain(ctx, pool, batch)
		if err != nil && ctx.Err() == nil {
			// Log and carry on: whatever was skipped is picked up on the next tick.
			log.Printf("sweep: %v", err)
		}
		if n > 0 {
			log.Printf("expired %d reservation(s)", n)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			log.Print("stopping")
			return
		}
	}
}
