package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"stocklock/internal/sweep"
)

// sweepOnce runs a single sweep batch and fails the test on any error.
func (a *testAPI) sweepOnce(batch int) int {
	a.t.Helper()
	n, err := sweep.Once(context.Background(), a.pool, batch)
	if err != nil {
		a.t.Fatalf("sweep: %v", err)
	}
	return n
}

// wantNoExpiredHolds asserts that, right now, no RESERVED row for the SKU is past its
// deadline. True only after a sweep has run with nothing racing it.
func (a *testAPI) wantNoExpiredHolds() {
	a.t.Helper()
	var n int
	if err := a.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM reservations
		 WHERE sku = $1 AND state = 'RESERVED' AND deadline <= clock_timestamp()`, a.sku).Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	if n != 0 {
		a.t.Errorf("%d RESERVED rows are past their deadline after sweeping, want 0", n)
	}
}

// Basics. 5 reservations, 3 of them past their deadline. One sweep expires exactly
// those 3 and returns their units; the 2 live ones are untouched. Sweeping again
// finds nothing.
func TestSweepExpiresOnlyOverdueReservations(t *testing.T) {
	a := newTestAPI(t, 10)
	var overdue, live []string
	for range 3 {
		id := a.reserve()
		a.backdate(id)
		overdue = append(overdue, id)
	}
	for range 2 {
		live = append(live, a.reserve())
	}
	a.wantInventory(5, 5, 0)

	if n := a.sweepOnce(100); n != 3 {
		t.Errorf("first sweep expired %d, want 3", n)
	}
	for _, id := range overdue {
		a.wantState(id, "EXPIRED")
	}
	for _, id := range live {
		a.wantState(id, "RESERVED")
	}
	a.wantInventory(8, 2, 0)
	a.wantReservationRows("EXPIRED", 3)
	a.wantNoExpiredHolds()

	if n := a.sweepOnce(100); n != 0 {
		t.Errorf("second sweep expired %d, want 0", n)
	}
	a.wantInventory(8, 2, 0)
}

// One batch can span several SKUs. Each stock row must move by exactly the number
// of ITS reservations that expired, not by the batch total.
func TestSweepHandlesSeveralSKUsInOneBatch(t *testing.T) {
	a := newTestAPI(t, 10)
	b := newTestAPI(t, 10)
	for range 3 {
		a.backdate(a.reserve())
	}
	for range 1 {
		b.backdate(b.reserve())
	}
	b.reserve() // live

	if n := a.sweepOnce(100); n != 4 {
		t.Errorf("sweep expired %d, want 4", n)
	}
	a.wantInventory(10, 0, 0)
	b.wantInventory(9, 1, 0)
}

// The batch size is a real limit: with 25 overdue reservations and batches of 10,
// one sweep handles 10, and Drain keeps going until everything is expired.
func TestSweepRespectsBatchSize(t *testing.T) {
	a := newTestAPI(t, 30)
	for range 25 {
		a.backdate(a.reserve())
	}

	if n := a.sweepOnce(10); n != 10 {
		t.Errorf("one batch expired %d, want 10", n)
	}
	a.wantInventory(15, 15, 0)

	total, err := sweep.Drain(context.Background(), a.pool, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 15 {
		t.Errorf("drain expired %d, want the remaining 15", total)
	}
	a.wantInventory(30, 0, 0)
	a.wantNoExpiredHolds()
}

// A shopper who arrives after the sweeper has already expired their reservation is
// told "expired", the same answer they would get for simply being late.
func TestFinishAfterSweepSaysExpired(t *testing.T) {
	a := newTestAPI(t, 10)
	id := a.reserve()
	a.backdate(id)
	a.sweepOnce(100)

	for _, action := range []string{"confirm", "abandon"} {
		code, body := a.finish(action, id)
		wantError(t, action, code, http.StatusConflict, body, "reservation_expired")
	}
	a.wantInventory(10, 0, 0)
}

// The heart of the phase: a finish and the sweeper collide right at the deadline.
// The deadline is set 50ms out; a confirm (or abandon) and a burst of sweeps are
// fired at the same instant. There are exactly two legal endings, never a mix:
//
//	shopper won:  200, row CONFIRMED (9/0/1) or ABANDONED (10/0/0)
//	sweeper won:  409 reservation_expired, row EXPIRED, 10/0/0
//
// A 500 (which is what a deadlock victim would produce) fails the round.
func TestFinishRacesSweeperAtDeadline(t *testing.T) {
	for _, action := range []string{"confirm", "abandon"} {
		t.Run(action, func(t *testing.T) {
			shopperWins, sweeperWins := 0, 0
			rounds(t, 50, func(t *testing.T) {
				a := newTestAPI(t, 10)
				id := a.reserve()
				if _, err := a.pool.Exec(context.Background(),
					`UPDATE reservations SET deadline = clock_timestamp() + interval '50 milliseconds'
					 WHERE reservation_id = $1`, id); err != nil {
					t.Fatal(err)
				}

				var sweepErr error
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					// Sweep continuously for a while, so sweeps land on both sides of the deadline.
					for start := time.Now(); time.Since(start) < 150*time.Millisecond; {
						if _, err := sweep.Once(context.Background(), a.pool, 100); err != nil {
							sweepErr = err
							return
						}
					}
				}()
				// Delay the shopper by a random-ish slice of the window so the collision
				// happens at different moments in different rounds.
				time.Sleep(time.Duration(t.Name()[len(t.Name())-1]%7) * 12 * time.Millisecond)
				code, body, err := a.request("POST", "/"+action, finishBody(newOpID(), id))
				wg.Wait()

				if err != nil {
					t.Fatal(err)
				}
				if sweepErr != nil {
					t.Fatalf("sweep failed: %v", sweepErr)
				}
				switch code {
				case http.StatusOK:
					shopperWins++
					if action == "confirm" {
						a.wantState(id, "CONFIRMED")
						a.wantInventory(9, 0, 1)
					} else {
						a.wantState(id, "ABANDONED")
						a.wantInventory(10, 0, 0)
					}
				case http.StatusConflict:
					sweeperWins++
					if body["error"] != "reservation_expired" {
						t.Errorf("body = %v, want reservation_expired", body)
					}
					a.wantState(id, "EXPIRED")
					a.wantInventory(10, 0, 0)
				default:
					t.Fatalf("status %d, body %v: neither outcome", code, body)
				}
			})
			t.Logf("%s: shopper won %d rounds, sweeper won %d rounds", action, shopperWins, sweeperWins)
			if shopperWins == 0 || sweeperWins == 0 {
				t.Errorf("both sides must win sometimes for this race to mean anything (shopper %d, sweeper %d)",
					shopperWins, sweeperWins)
			}
		})
	}
}

// The sweeper running continuously under mixed traffic with a short hold window.
// 30 units, 40 shoppers reserve at once; the winners then confirm, abandon, or
// simply disappear. With the sweeper running in the background throughout, once
// everything settles: the invariants hold, nobody sold or abandoned more than they
// held, and every disappeared hold has been expired and returned.
func TestSweeperUnderMixedTraffic(t *testing.T) {
	a := newTestAPI(t, 30)
	a.handler = (&server{pool: a.pool, holdWindow: 300 * time.Millisecond}).routes()

	ctx, cancel := context.WithCancel(context.Background())
	sweeperDone := make(chan error, 1)
	go func() {
		for {
			if _, err := sweep.Once(ctx, a.pool, 100); err != nil && ctx.Err() == nil {
				sweeperDone <- err
				return
			}
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				sweeperDone <- nil
				return
			}
		}
	}()

	reserves := fireTogether(40, func(int) (int, map[string]any, error) { return a.reserveRequest() })
	var held []string
	for _, r := range reserves {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.code == http.StatusCreated {
			held = append(held, r.body["reservation_id"].(string))
		}
	}
	if len(held) != 30 {
		t.Fatalf("%d reserves succeeded, want 30", len(held))
	}

	// Shopper i: confirm, abandon, or disappear, by i % 3. Some are quick, some are late.
	finishes := fireTogether(30, func(i int) (int, map[string]any, error) {
		if i%3 == 2 {
			return http.StatusNoContent, map[string]any{}, nil // disappears
		}
		if i%2 == 0 {
			time.Sleep(400 * time.Millisecond) // late: past the 300ms hold
		}
		action := "confirm"
		if i%3 == 1 {
			action = "abandon"
		}
		return a.finishRequest(action, held[i])
	})

	confirmed, abandoned := 0, 0
	for i, r := range finishes {
		if r.err != nil {
			t.Fatal(r.err)
		}
		switch {
		case i%3 == 2:
		case r.code == http.StatusOK && i%3 == 0:
			confirmed++
		case r.code == http.StatusOK:
			abandoned++
		case r.code == http.StatusConflict && r.body["error"] == "reservation_expired":
		default:
			t.Errorf("finish %d: status %d, body %v", i, r.code, r.body)
		}
	}

	// Let every remaining hold expire and get swept, then stop the sweeper.
	time.Sleep(600 * time.Millisecond)
	cancel()
	if err := <-sweeperDone; err != nil {
		t.Fatalf("sweeper: %v", err)
	}
	a.sweepOnce(100) // one final pass with nothing racing it

	a.wantInventory(30-confirmed, 0, confirmed)
	a.wantReservationRows("CONFIRMED", confirmed)
	a.wantReservationRows("ABANDONED", abandoned)
	a.wantReservationRows("EXPIRED", 30-confirmed-abandoned)
	a.wantReservationRows("RESERVED", 0)
	a.wantNoExpiredHolds()
	t.Logf("confirmed %d, abandoned %d, expired %d", confirmed, abandoned, 30-confirmed-abandoned)
}

// The same collision, but crowded: 20 reservations on ONE SKU all expiring at the
// same moment, 20 shoppers finishing them spread across that moment, and the
// sweeper batching in the background. Every reservation must end CONFIRMED (shopper
// won) or EXPIRED (sweeper won), the counters must add up, and no request may fail
// with a 500, which is what a deadlock victim produces. The sweeper holds a whole
// batch of reservation rows while it takes the stock row, so a wrong lock order
// anywhere would show up here.
func TestCrowdedDeadlineOnOneSKU(t *testing.T) {
	// 30 rounds: with a wrong lock order, deadlocks appeared in roughly half of all
	// rounds when this was checked, so 30 makes a miss vanishingly unlikely.
	rounds(t, 30, func(t *testing.T) {
		a := newTestAPI(t, 20)
		var ids []string
		for range 20 {
			ids = append(ids, a.reserve())
		}
		if _, err := a.pool.Exec(context.Background(),
			`UPDATE reservations SET deadline = clock_timestamp() + interval '50 milliseconds'
			 WHERE sku = $1`, a.sku); err != nil {
			t.Fatal(err)
		}

		var sweepErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for start := time.Now(); time.Since(start) < 200*time.Millisecond; {
				if _, err := sweep.Once(context.Background(), a.pool, 100); err != nil {
					sweepErr = err
					return
				}
			}
		}()
		results := fireTogether(20, func(i int) (int, map[string]any, error) {
			time.Sleep(time.Duration(i) * 5 * time.Millisecond) // spread across the deadline
			return a.finishRequest("confirm", ids[i])
		})
		wg.Wait()
		if sweepErr != nil {
			t.Fatalf("sweep failed: %v", sweepErr)
		}

		confirmed := 0
		for i, r := range results {
			if r.err != nil {
				t.Fatal(r.err)
			}
			switch {
			case r.code == http.StatusOK:
				confirmed++
			case r.code == http.StatusConflict && r.body["error"] == "reservation_expired":
			default:
				t.Errorf("confirm %d: status %d, body %v", i, r.code, r.body)
			}
		}
		a.wantInventory(20-confirmed, 0, confirmed)
		a.wantReservationRows("CONFIRMED", confirmed)
		a.wantReservationRows("EXPIRED", 20-confirmed)
	})
}
