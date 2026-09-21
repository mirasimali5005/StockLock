// Package invariant verifies the inventory invariants against the database.
// It is used by the checker command and by tests that mutate inventory.
package invariant

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Violation kinds, one per invariant.
const (
	KindUnitsNotConserved = "UNITS_NOT_CONSERVED" // available + reserved + sold != initial_stock
	KindNegativeAvailable = "NEGATIVE_AVAILABLE"  // available < 0
	KindReservedMismatch  = "RESERVED_MISMATCH"   // reserved != number of RESERVED reservation rows
	KindSoldMismatch      = "SOLD_MISMATCH"       // sold != number of CONFIRMED reservation rows
)

type Violation struct {
	SKU    string
	Kind   string
	Detail string
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

func checkSKU(s skuState) []Violation {
	var vs []Violation
	add := func(kind, format string, args ...any) {
		vs = append(vs, Violation{SKU: s.sku, Kind: kind, Detail: fmt.Sprintf(format, args...)})
	}

	if sum := s.available + s.reserved + s.sold; sum != s.initialStock {
		add(KindUnitsNotConserved, "available(%d) + reserved(%d) + sold(%d) = %d, want initial_stock %d",
			s.available, s.reserved, s.sold, sum, s.initialStock)
	}
	if s.available < 0 {
		add(KindNegativeAvailable, "available = %d", s.available)
	}
	if s.reserved != s.reservedRows {
		add(KindReservedMismatch, "stock.reserved = %d but %d RESERVED reservation rows exist",
			s.reserved, s.reservedRows)
	}
	if s.sold != s.confirmedRows {
		add(KindSoldMismatch, "stock.sold = %d but %d CONFIRMED reservation rows exist",
			s.sold, s.confirmedRows)
	}
	return vs
}

// Check returns every invariant violation visible to tx and the number of SKUs examined.
func Check(ctx context.Context, tx pgx.Tx) ([]Violation, int, error) {
	rows, err := tx.Query(ctx, stateQuery)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var vs []Violation
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
