package main

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

// wantOperationRows checks how many rows the operations table holds for one operation id.
func (a *testAPI) wantOperationRows(opID string, want int) {
	a.t.Helper()
	var got int
	if err := a.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM operations WHERE operation_id = $1`, opID).Scan(&got); err != nil {
		a.t.Fatal(err)
	}
	if got != want {
		a.t.Errorf("operation rows for %s = %d, want %d", opID, got, want)
	}
}

// wantAllSame checks that every caller got the same status and the same body.
func wantAllSame(t *testing.T, results []result, wantCode int) map[string]any {
	t.Helper()
	for i, r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.code != wantCode {
			t.Fatalf("caller %d: status %d, body %v, want %d", i, r.code, r.body, wantCode)
		}
		if !reflect.DeepEqual(r.body, results[0].body) {
			t.Fatalf("caller %d got %v but caller 0 got %v", i, r.body, results[0].body)
		}
	}
	return results[0].body
}

// The lost-response story: a shopper reserves, never sees the answer, and sends the
// exact same request again. The retry must get the same reservation back and the
// stock must not move a second time.
func TestRetriedReserveReturnsSameReservation(t *testing.T) {
	a := newTestAPI(t, 10)
	opID := newOpID()

	code1, first := a.do("POST", "/reserve", reserveBody(opID, a.sku))
	wantStatus(t, "first attempt", code1, http.StatusCreated, first)
	a.wantInventory(9, 1, 0)

	code2, retry := a.do("POST", "/reserve", reserveBody(opID, a.sku))
	wantStatus(t, "retry", code2, http.StatusCreated, retry)
	if !reflect.DeepEqual(first, retry) {
		t.Errorf("retry body = %v, want the original %v", retry, first)
	}
	a.wantInventory(9, 1, 0)
	a.wantReservationRows("RESERVED", 1)
	a.wantOperationRows(opID, 1)
}

// THE headline test of this phase: the same operation id arrives 50 times at the
// same instant. All 50 callers must get 201 with the same reservation id, and the
// database must show exactly one reservation and one unit moved.
func TestFiftyConcurrentRetriesReserveOnce(t *testing.T) {
	rounds(t, 10, func(t *testing.T) {
		a := newTestAPI(t, 10)
		opID := newOpID()
		results := fireTogether(50, func(int) (int, map[string]any, error) {
			return a.request("POST", "/reserve", reserveBody(opID, a.sku))
		})

		body := wantAllSame(t, results, http.StatusCreated)
		if body["reservation_id"] == nil {
			t.Errorf("body = %v, want a reservation_id", body)
		}
		a.wantInventory(9, 1, 0)
		a.wantReservationRows("RESERVED", 1)
		a.wantOperationRows(opID, 1)
	})
}

// Same idea for confirm and abandon: one operation id fired 50 times at once.
// Every caller gets 200 (nobody sees "already finished", because they are all the
// same operation), and the unit moves exactly once.
func TestFiftyConcurrentRetriesFinishOnce(t *testing.T) {
	cases := []struct {
		action                    string
		available, reserved, sold int
	}{
		{"confirm", 9, 0, 1},
		{"abandon", 10, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			rounds(t, 10, func(t *testing.T) {
				a := newTestAPI(t, 10)
				id := a.reserve()
				opID := newOpID()
				results := fireTogether(50, func(int) (int, map[string]any, error) {
					return a.request("POST", "/"+tc.action, finishBody(opID, id))
				})

				wantAllSame(t, results, http.StatusOK)
				a.wantInventory(tc.available, tc.reserved, tc.sold)
				a.wantOperationRows(opID, 1)
			})
		})
	}
}

// A refusal is an answer too, and it is remembered. A reserve that was told
// "out of stock" stays out of stock on retry, even though a unit has come back in
// the meantime. A brand-new operation id, however, can take that unit.
func TestRefusalIsReplayedEvenAfterStockReturns(t *testing.T) {
	a := newTestAPI(t, 1)
	held := a.reserve()

	opID := newOpID()
	code, body := a.do("POST", "/reserve", reserveBody(opID, a.sku))
	wantError(t, "first attempt", code, http.StatusConflict, body, "out_of_stock")

	a.finish("abandon", held) // the unit is available again

	code, body = a.do("POST", "/reserve", reserveBody(opID, a.sku))
	wantError(t, "retry", code, http.StatusConflict, body, "out_of_stock")
	a.wantInventory(1, 0, 0)

	a.reserve() // a new operation id succeeds
	a.wantInventory(0, 1, 0)
}

// A retried confirm gets 200 again rather than 409 "already finished": it is the
// same operation, so it gets the same answer. A different operation id confirming
// the same reservation is a different request, and that one is refused.
func TestRetriedConfirmIsNotAConflict(t *testing.T) {
	a := newTestAPI(t, 10)
	id := a.reserve()
	opID := newOpID()

	for _, attempt := range []string{"first attempt", "retry"} {
		code, body := a.do("POST", "/confirm", finishBody(opID, id))
		wantStatus(t, attempt, code, http.StatusOK, body)
	}
	code, body := a.do("POST", "/confirm", finishBody(newOpID(), id))
	wantError(t, "different operation", code, http.StatusConflict, body, "reservation_not_reserved")
	a.wantInventory(9, 0, 1)
}

// An operation id belongs to one exact request. Reusing it for a different action
// or a different target is a client bug: it gets 422 and changes nothing.
func TestOperationIDReuseIsRejected(t *testing.T) {
	a := newTestAPI(t, 10)
	other := newTestAPI(t, 10)
	opID := newOpID()

	code, body := a.do("POST", "/reserve", reserveBody(opID, a.sku))
	wantStatus(t, "reserve", code, http.StatusCreated, body)
	id := body["reservation_id"].(string)

	code, body = a.do("POST", "/reserve", reserveBody(opID, other.sku))
	wantError(t, "same id, different sku", code, http.StatusUnprocessableEntity, body, "operation_id_reuse")

	code, body = a.do("POST", "/confirm", finishBody(opID, id))
	wantError(t, "same id, different action", code, http.StatusUnprocessableEntity, body, "operation_id_reuse")

	a.wantInventory(9, 1, 0)
	other.wantInventory(10, 0, 0)
}

// Idempotency must not weaken Phase 3: two DIFFERENT shoppers, each retrying their
// own operation id 10 times at once, fight for the last unit. One shopper wins and
// all 10 of their copies say 201; all 10 of the loser's copies say 409.
func TestTwoRetryingShoppersLastUnit(t *testing.T) {
	rounds(t, 20, func(t *testing.T) {
		a := newTestAPI(t, 1)
		ops := []string{newOpID(), newOpID()}
		results := fireTogether(20, func(i int) (int, map[string]any, error) {
			return a.request("POST", "/reserve", reserveBody(ops[i%2], a.sku))
		})

		winners := 0
		for shopper := range 2 {
			var theirs []result
			for i, r := range results {
				if i%2 == shopper {
					theirs = append(theirs, r)
				}
			}
			if theirs[0].code == http.StatusCreated {
				winners++
			}
			wantAllSame(t, theirs, theirs[0].code)
		}
		if winners != 1 {
			t.Errorf("shoppers who got the unit = %d, want exactly 1", winners)
		}
		a.wantInventory(0, 1, 0)
		a.wantReservationRows("RESERVED", 1)
	})
}
