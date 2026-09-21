package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// backdate moves a reservation's deadline into the past, so it has expired without
// the test having to sleep through a real hold window.
func (a *testAPI) backdate(reservationID string) {
	a.t.Helper()
	if _, err := a.pool.Exec(context.Background(),
		`UPDATE reservations SET deadline = now() - interval '1 second' WHERE reservation_id = $1`,
		reservationID); err != nil {
		a.t.Fatal(err)
	}
}

// wantState checks a reservation's state straight from the database.
func (a *testAPI) wantState(reservationID, want string) {
	a.t.Helper()
	var got string
	if err := a.pool.QueryRow(context.Background(),
		`SELECT state FROM reservations WHERE reservation_id = $1`, reservationID).Scan(&got); err != nil {
		a.t.Fatal(err)
	}
	if got != want {
		a.t.Errorf("reservation state = %s, want %s", got, want)
	}
}

// Before the deadline, confirm and abandon work exactly as in Phase 2.
func TestFinishBeforeDeadlineSucceeds(t *testing.T) {
	a := newTestAPI(t, 10)
	id := a.reserve()
	code, body := a.finish("confirm", id)
	wantStatus(t, "confirm", code, http.StatusOK, body)
	a.wantInventory(9, 0, 1)
}

// Past the deadline, both confirm and abandon are refused with 409
// reservation_expired. The row stays RESERVED and the unit stays held: returning it
// is the sweeper's job (Phase 6), not the shopper's.
func TestFinishAfterDeadlineIsRefused(t *testing.T) {
	for _, action := range []string{"confirm", "abandon"} {
		t.Run(action, func(t *testing.T) {
			a := newTestAPI(t, 10)
			id := a.reserve()
			a.backdate(id)

			code, body := a.finish(action, id)
			wantError(t, action, code, http.StatusConflict, body, "reservation_expired")
			a.wantState(id, "RESERVED")
			a.wantInventory(9, 1, 0)
		})
	}
}

// The roadmap's exit test, in real time: a 1-second hold, reserve, wait for it to
// pass, confirm. It must fail with no cleanup process involved at all.
func TestConfirmAfterRealHoldWindowFails(t *testing.T) {
	a := newTestAPI(t, 10)
	a.handler = (&server{pool: a.pool, holdWindow: time.Second}).routes()

	id := a.reserve()
	time.Sleep(1200 * time.Millisecond)

	code, body := a.finish("confirm", id)
	wantError(t, "late confirm", code, http.StatusConflict, body, "reservation_expired")
	a.wantInventory(9, 1, 0)
}

// The now() trap. A confirm starts BEFORE the deadline but has to wait for the
// reservation row lock, and the deadline passes while it waits. When it finally gets
// the lock the deadline is behind it, so it must fail. Postgres's now() is frozen at
// transaction start and would let this confirm through; clock_timestamp() does not.
func TestConfirmThatWaitedPastDeadlineFails(t *testing.T) {
	ctx := context.Background()
	a := newTestAPI(t, 10)
	id := a.reserve()

	// Another transaction grabs the reservation row lock and keeps it.
	blocker, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx,
		`UPDATE reservations SET deadline = clock_timestamp() + interval '300 milliseconds'
		 WHERE reservation_id = $1`, id); err != nil {
		t.Fatal(err)
	}

	// The confirm starts now, well inside the deadline, and blocks on that lock.
	done := make(chan result, 1)
	go func() {
		code, body, err := a.request("POST", "/confirm", finishBody(newOpID(), id))
		done <- result{code, body, err}
	}()

	// Let the deadline pass, then release the lock.
	time.Sleep(600 * time.Millisecond)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	wantError(t, "confirm that waited", r.code, http.StatusConflict, r.body, "reservation_expired")
	a.wantState(id, "RESERVED")
	a.wantInventory(9, 1, 0)
}

// Expiry does not rewrite history. A confirm that succeeded in time still replays
// 200 when retried after the deadline, and a confirm refused as expired still
// replays 409 when retried. A brand-new operation on the expired reservation is
// refused too.
func TestDeadlineAnswersAreReplayed(t *testing.T) {
	a := newTestAPI(t, 10)

	confirmedInTime := a.reserve()
	okOp := newOpID()
	code, body := a.do("POST", "/confirm", finishBody(okOp, confirmedInTime))
	wantStatus(t, "confirm in time", code, http.StatusOK, body)
	a.backdate(confirmedInTime)
	code, body = a.do("POST", "/confirm", finishBody(okOp, confirmedInTime))
	wantStatus(t, "retry after deadline", code, http.StatusOK, body)

	tooLate := a.reserve()
	a.backdate(tooLate)
	lateOp := newOpID()
	for _, attempt := range []string{"late confirm", "its retry"} {
		code, body = a.do("POST", "/confirm", finishBody(lateOp, tooLate))
		wantError(t, attempt, code, http.StatusConflict, body, "reservation_expired")
	}
	code, body = a.do("POST", "/confirm", finishBody(newOpID(), tooLate))
	wantError(t, "new operation", code, http.StatusConflict, body, "reservation_expired")

	a.wantInventory(8, 1, 1)
}
