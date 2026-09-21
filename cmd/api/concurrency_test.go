package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// result is what one simulated shopper got back.
type result struct {
	code int
	body map[string]any
	err  error
}

// fireTogether runs n requests at the same moment. Every goroutine first waits at a
// shared start gate; closing the gate releases them all at once, so they genuinely
// collide instead of politely arriving one after another.
func fireTogether(n int, send func(i int) (int, map[string]any, error)) []result {
	results := make([]result, n)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			code, body, err := send(i)
			results[i] = result{code, body, err}
		}()
	}
	close(gate)
	wg.Wait()
	return results
}

// countCodes tallies results by HTTP status and fails the test on any broken response.
func countCodes(t *testing.T, results []result) map[int]int {
	t.Helper()
	counts := map[int]int{}
	for _, r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		counts[r.code]++
	}
	return counts
}

// reserveRequest and finishRequest each send a brand-new operation (fresh operation id),
// so these are different shoppers competing, not one shopper retrying.
func (a *testAPI) reserveRequest() (int, map[string]any, error) {
	return a.request("POST", "/reserve", reserveBody(newOpID(), a.sku))
}

func (a *testAPI) finishRequest(action, reservationID string) (int, map[string]any, error) {
	return a.request("POST", "/"+action, finishBody(newOpID(), reservationID))
}

// wantReservationRows checks how many reservation rows the SKU has in a given state,
// looking in the database directly rather than trusting what the API responded.
func (a *testAPI) wantReservationRows(state string, want int) {
	a.t.Helper()
	var got int
	err := a.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM reservations WHERE sku = $1 AND state = $2`, a.sku, state).Scan(&got)
	if err != nil {
		a.t.Fatal(err)
	}
	if got != want {
		a.t.Errorf("%s reservation rows = %d, want %d", state, got, want)
	}
}

// rounds repeats a scenario, each time on a brand-new SKU. A race bug may only show up
// once in many attempts, so one passing run proves very little.
func rounds(t *testing.T, n int, scenario func(t *testing.T)) {
	for i := range n {
		t.Run(fmt.Sprintf("round %d", i+1), scenario)
	}
}

// THE core claim of the project: one unit left, two shoppers reserve at the same
// instant. Exactly one must win (201) and the other must be told out of stock (409).
// Two winners would be an oversell.
func TestLastUnitHasExactlyOneWinner(t *testing.T) {
	rounds(t, 50, func(t *testing.T) {
		a := newTestAPI(t, 1)
		results := fireTogether(2, func(int) (int, map[string]any, error) { return a.reserveRequest() })

		codes := countCodes(t, results)
		if codes[http.StatusCreated] != 1 || codes[http.StatusConflict] != 1 {
			t.Errorf("status counts = %v, want one 201 and one 409", codes)
		}
		a.wantInventory(0, 1, 0)
		a.wantReservationRows("RESERVED", 1)
	})
}

// 100 shoppers rush 50 units. Exactly 50 must succeed and 50 must be refused.
// It also checks the "responses match reality" rule: the 50 successes carry 50
// different reservation ids, and exactly 50 reservation rows exist in the database.
func TestHundredShoppersFiftyUnits(t *testing.T) {
	rounds(t, 10, func(t *testing.T) {
		a := newTestAPI(t, 50)
		results := fireTogether(100, func(int) (int, map[string]any, error) { return a.reserveRequest() })

		codes := countCodes(t, results)
		if codes[http.StatusCreated] != 50 || codes[http.StatusConflict] != 50 {
			t.Errorf("status counts = %v, want fifty 201s and fifty 409s", codes)
		}
		ids := map[string]bool{}
		for _, r := range results {
			if r.code == http.StatusCreated {
				ids[r.body["reservation_id"].(string)] = true
			} else if r.body["error"] != "out_of_stock" {
				t.Errorf("refusal body = %v, want out_of_stock", r.body)
			}
		}
		if len(ids) != 50 {
			t.Errorf("distinct reservation ids handed out = %d, want 50", len(ids))
		}
		a.wantInventory(0, 50, 0)
		a.wantReservationRows("RESERVED", 50)
	})
}

// One reservation gets hit by 10 finish requests at the same instant (think: a
// double-clicked button, or confirm racing a cancel). Exactly one may succeed, the
// other nine get 409, and the unit moves exactly once. The "mixed" case alternates
// confirm and abandon, so either may win, but never both.
func TestRacingFinishesHaveExactlyOneWinner(t *testing.T) {
	cases := []struct {
		name    string
		actions []string // request i uses actions[i % len(actions)]
	}{
		{"confirm vs confirm", []string{"confirm"}},
		{"abandon vs abandon", []string{"abandon"}},
		{"confirm vs abandon", []string{"confirm", "abandon"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rounds(t, 20, func(t *testing.T) {
				a := newTestAPI(t, 10)
				id := a.reserve()

				results := fireTogether(10, func(i int) (int, map[string]any, error) {
					return a.finishRequest(tc.actions[i%len(tc.actions)], id)
				})

				codes := countCodes(t, results)
				if codes[http.StatusOK] != 1 || codes[http.StatusConflict] != 9 {
					t.Fatalf("status counts = %v, want one 200 and nine 409s", codes)
				}
				// The final counts must agree with whichever request won.
				for _, r := range results {
					if r.code != http.StatusOK {
						continue
					}
					if r.body["state"] == "CONFIRMED" {
						a.wantInventory(9, 0, 1)
					} else {
						a.wantInventory(10, 0, 0)
					}
				}
			})
		})
	}
}

// Everything at once on one SKU. 30 units, 20 already held. Then simultaneously:
// 10 shoppers confirm, 10 others abandon, and 30 new shoppers try to reserve.
// All 20 finishes must succeed. New reserves compete for the 10 free units plus
// whatever the abandons hand back in time, so between 10 and 20 succeed. The exact
// number depends on timing, but the final counts must add up to exactly what the
// responses said happened.
func TestMixedTrafficOnOneSKU(t *testing.T) {
	rounds(t, 10, func(t *testing.T) {
		a := newTestAPI(t, 30)
		var held []string
		for range 20 {
			held = append(held, a.reserve())
		}

		results := fireTogether(50, func(i int) (int, map[string]any, error) {
			switch {
			case i < 10:
				return a.finishRequest("confirm", held[i])
			case i < 20:
				return a.finishRequest("abandon", held[i])
			default:
				return a.reserveRequest()
			}
		})

		newReserves := 0
		for i, r := range results {
			if r.err != nil {
				t.Fatal(r.err)
			}
			switch {
			case i < 20:
				if r.code != http.StatusOK {
					t.Errorf("finish %d: status %d, body %v, want 200", i, r.code, r.body)
				}
			case r.code == http.StatusCreated:
				newReserves++
			case r.code != http.StatusConflict:
				t.Errorf("reserve %d: status %d, body %v, want 201 or 409", i, r.code, r.body)
			}
		}
		if newReserves < 10 || newReserves > 20 {
			t.Errorf("new reserves = %d, want between 10 and 20", newReserves)
		}

		// 10 sold, 10 abandoned, and every successful new reserve is still held.
		a.wantInventory(30-10-newReserves, newReserves, 10)
		a.wantReservationRows("RESERVED", newReserves)
		a.wantReservationRows("CONFIRMED", 10)
		a.wantReservationRows("ABANDONED", 10)
	})
}
