// Package sweep expires reservations whose deadline has passed and returns their
// units to available stock.
package sweep

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// DB is the subset of pgxpool.Pool that Sweep needs.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Once expires up to batchSize reservations in one transaction and returns how many
// it expired. A short result (fewer than batchSize) means there is nothing more to do
// right now.
//
// Lock order is the same as confirm and abandon: reservation rows first, then stock
// rows. SKIP LOCKED leaves any reservation that a confirm or abandon is currently
// holding for the next sweep instead of queueing behind it.
func Once(ctx context.Context, db DB, batchSize int) (int, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Postgres re-checks the WHERE clause after acquiring each row lock, so a row that
	// was confirmed or abandoned a moment ago drops out of the batch by itself.
	rows, err := tx.Query(ctx,
		`SELECT reservation_id FROM reservations
		 WHERE state = 'RESERVED' AND deadline <= clock_timestamp()
		 ORDER BY deadline
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, batchSize)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[[16]byte])
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE reservations SET state = 'EXPIRED' WHERE reservation_id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	// One stock update per SKU, by however many of its reservations just expired.
	if _, err := tx.Exec(ctx,
		`UPDATE stock s
		 SET reserved = s.reserved - c.n, available = s.available + c.n
		 FROM (SELECT sku, COUNT(*) AS n FROM reservations WHERE reservation_id = ANY($1) GROUP BY sku) c
		 WHERE s.sku = c.sku`, ids); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit(ctx)
}

// Drain calls Once until a batch comes back short, and returns the total expired.
func Drain(ctx context.Context, db DB, batchSize int) (int, error) {
	total := 0
	for {
		n, err := Once(ctx, db, batchSize)
		total += n
		if err != nil || n < batchSize {
			return total, err
		}
	}
}
