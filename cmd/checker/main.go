// Command checker verifies the inventory invariants against the database.
// It exits 0 when every SKU is consistent and 1 when any invariant is violated.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

const defaultDatabaseURL = "postgres://stocklock:stocklock@localhost:5432/stocklock"

// Violation kinds, one per invariant.
const (
	kindUnitsNotConserved = "UNITS_NOT_CONSERVED" // available + reserved + sold != initial_stock
	kindNegativeAvailable = "NEGATIVE_AVAILABLE"  // available < 0
	kindReservedMismatch  = "RESERVED_MISMATCH"   // reserved != number of RESERVED reservation rows
	kindSoldMismatch      = "SOLD_MISMATCH"       // sold != number of CONFIRMED reservation rows
)

type violation struct {
	sku    string
	kind   string
	detail string
}

// skuState is one SKU's counters next to the reservation rows that should explain them.
type skuState struct {
	sku           string
	initialStock  int
	available     int
	reserved      int
	sold          int
	reservedRows  int
	confirmedRows int
}

// A single statement, so counters and reservation rows come from the same snapshot
// even while other transactions are running.
const stateQuery = `
SELECT s.sku, s.initial_stock, s.available, s.reserved, s.sold,
       COUNT(r.reservation_id) FILTER (WHERE r.state = 'RESERVED'),
       COUNT(r.reservation_id) FILTER (WHERE r.state = 'CONFIRMED')
FROM stock s
LEFT JOIN reservations r ON r.sku = s.sku
GROUP BY s.sku
ORDER BY s.sku`

func checkSKU(s skuState) []violation {
	var vs []violation
	add := func(kind, format string, args ...any) {
		vs = append(vs, violation{sku: s.sku, kind: kind, detail: fmt.Sprintf(format, args...)})
	}

	if sum := s.available + s.reserved + s.sold; sum != s.initialStock {
		add(kindUnitsNotConserved, "available(%d) + reserved(%d) + sold(%d) = %d, want initial_stock %d",
			s.available, s.reserved, s.sold, sum, s.initialStock)
	}
	if s.available < 0 {
		add(kindNegativeAvailable, "available = %d", s.available)
	}
	if s.reserved != s.reservedRows {
		add(kindReservedMismatch, "stock.reserved = %d but %d RESERVED reservation rows exist",
			s.reserved, s.reservedRows)
	}
	if s.sold != s.confirmedRows {
		add(kindSoldMismatch, "stock.sold = %d but %d CONFIRMED reservation rows exist",
			s.sold, s.confirmedRows)
	}
	return vs
}

// check returns every invariant violation visible to tx and the number of SKUs examined.
func check(ctx context.Context, tx pgx.Tx) ([]violation, int, error) {
	rows, err := tx.Query(ctx, stateQuery)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var vs []violation
	skus := 0
	for rows.Next() {
		var s skuState
		if err := rows.Scan(&s.sku, &s.initialStock, &s.available, &s.reserved, &s.sold,
			&s.reservedRows, &s.confirmedRows); err != nil {
			return nil, 0, err
		}
		skus++
		vs = append(vs, checkSKU(s)...)
	}
	return vs, skus, rows.Err()
}

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

	vs, skus, err := check(ctx, tx)
	if err != nil {
		return 2, err
	}
	if len(vs) == 0 {
		fmt.Printf("OK: %d SKU(s) checked, all invariants hold\n", skus)
		return 0, nil
	}
	for _, v := range vs {
		fmt.Printf("VIOLATION %s %s: %s\n", v.sku, v.kind, v.detail)
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
