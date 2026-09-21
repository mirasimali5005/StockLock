// Command checker verifies the inventory invariants against the database.
// It exits 0 when every SKU is consistent and 1 when any invariant is violated.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"

	"stocklock/internal/invariant"
)

const defaultDatabaseURL = "postgres://stocklock:stocklock@localhost:5432/stocklock"

func run(ctx context.Context) (int, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = defaultDatabaseURL
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return 2, err
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return 2, err
	}
	defer tx.Rollback(ctx)

	vs, skus, err := invariant.Check(ctx, tx)
	if err != nil {
		return 2, err
	}
	if len(vs) == 0 {
		fmt.Printf("OK: %d SKU(s) checked, all invariants hold\n", skus)
		return 0, nil
	}
	for _, v := range vs {
		fmt.Printf("VIOLATION %s %s: %s\n", v.SKU, v.Kind, v.Detail)
	}
	fmt.Printf("FAIL: %d violation(s) across %d SKU(s) checked\n", len(vs), skus)
	return 1, nil
}

func main() {
	code, err := run(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "checker:", err)
	}
	os.Exit(code)
}
