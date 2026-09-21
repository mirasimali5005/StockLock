package main

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

const testSKU = "TEST-SKU"

// Each case builds a state for testSKU (initial_stock 10) inside a transaction that is
// rolled back, runs the checker inside that same transaction, and compares violation kinds.
func TestCheck(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = defaultDatabaseURL
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Skipf("database not reachable (run `docker compose up -d`): %v", err)
	}
	defer conn.Close(ctx)

	type counts struct{ available, reserved, sold int }
	cases := []struct {
		name   string
		counts counts
		states []string // one reservation row per entry
		want   []string
	}{
		{
			name:   "untouched stock is consistent",
			counts: counts{10, 0, 0},
		},
		{
			name:   "one reservation in every state is consistent",
			counts: counts{8, 1, 1},
			states: []string{"RESERVED", "CONFIRMED", "ABANDONED", "EXPIRED"},
		},
		{
			name:   "a unit vanished",
			counts: counts{9, 0, 0},
			want:   []string{kindUnitsNotConserved},
		},
		{
			name:   "a unit was created",
			counts: counts{10, 1, 0},
			states: []string{"RESERVED"},
			want:   []string{kindUnitsNotConserved},
		},
		{
			// 11 holds on 10 units. The sum still equals 10, so only the sign check sees it.
			name:   "oversell",
			counts: counts{-1, 11, 0},
			states: slices.Repeat([]string{"RESERVED"}, 11),
			want:   []string{kindNegativeAvailable},
		},
		{
			// One reservation moved two units. The sum still equals 10.
			name:   "double reserve for one reservation",
			counts: counts{8, 2, 0},
			states: []string{"RESERVED"},
			want:   []string{kindReservedMismatch},
		},
		{
			// A reservation row was written without moving a unit.
			name:   "reservation row without a counter change",
			counts: counts{10, 0, 0},
			states: []string{"RESERVED"},
			want:   []string{kindReservedMismatch},
		},
		{
			// Confirm moved reserved -> sold twice for one reservation.
			name:   "double confirm for one reservation",
			counts: counts{8, 0, 2},
			states: []string{"CONFIRMED"},
			want:   []string{kindSoldMismatch},
		},
		{
			// Abandon marked the row but never returned the unit.
			name:   "abandon did not return the unit",
			counts: counts{9, 1, 0},
			states: []string{"ABANDONED"},
			want:   []string{kindReservedMismatch},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)

			if _, err := tx.Exec(ctx,
				`INSERT INTO stock (sku, initial_stock, available, reserved, sold) VALUES ($1, 10, $2, $3, $4)`,
				testSKU, tc.counts.available, tc.counts.reserved, tc.counts.sold); err != nil {
				t.Fatal(err)
			}
			for _, state := range tc.states {
				if _, err := tx.Exec(ctx,
					`INSERT INTO reservations (sku, state, deadline) VALUES ($1, $2, now() + interval '5 minutes')`,
					testSKU, state); err != nil {
					t.Fatal(err)
				}
			}

			vs, _, err := check(ctx, tx)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, v := range vs {
				if v.sku == testSKU {
					got = append(got, v.kind)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("violations = %v, want %v", got, tc.want)
			}
		})
	}
}
