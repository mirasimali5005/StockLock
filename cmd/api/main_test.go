package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"stocklock/internal/invariant"
)

// testAPI drives the real handlers against the real database, using a SKU of its own.
type testAPI struct {
	t       *testing.T
	pool    *pgxpool.Pool
	handler http.Handler
	sku     string
}

var (
	testPoolOnce sync.Once
	testPool     *pgxpool.Pool
	testPoolErr  error
)

// sharedPool returns one connection pool used by every test, with all of its
// connections already open. This matters for the concurrency tests: opening a
// connection takes milliseconds, so with a cold pool "simultaneous" requests would
// actually reach Postgres one after another and never really race.
func sharedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testPoolOnce.Do(func() {
		ctx := context.Background()
		url := os.Getenv("DATABASE_URL")
		if url == "" {
			url = defaultDatabaseURL
		}
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			testPoolErr = err
			return
		}
		// Hold every connection at the same time so the pool has to open them all.
		var conns []*pgxpool.Conn
		for range pool.Config().MaxConns {
			c, err := pool.Acquire(ctx)
			if err != nil {
				testPoolErr = err
				break
			}
			conns = append(conns, c)
		}
		for _, c := range conns {
			c.Release()
		}
		testPool = pool
	})
	if testPoolErr != nil {
		t.Skipf("database not reachable (run `docker compose up -d`): %v", testPoolErr)
	}
	return testPool
}

func newTestAPI(t *testing.T, initialStock int) *testAPI {
	t.Helper()
	ctx := context.Background()
	pool := sharedPool(t)

	sku := fmt.Sprintf("TEST-%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano())
	if _, err := pool.Exec(ctx,
		`INSERT INTO stock (sku, initial_stock, available, reserved, sold) VALUES ($1, $2, $2, 0, 0)`,
		sku, initialStock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM operations WHERE request = $1
			OR request IN (SELECT reservation_id::text FROM reservations WHERE sku = $1)`, sku)
		pool.Exec(ctx, `DELETE FROM reservations WHERE sku = $1`, sku)
		pool.Exec(ctx, `DELETE FROM stock WHERE sku = $1`, sku)
	})
	return &testAPI{t: t, pool: pool, sku: sku,
		handler: (&server{pool: pool, holdWindow: defaultHoldWindow}).routes()}
}

// request sends a request and returns the status and decoded JSON body.
// It never touches testing.T, so it is safe to call from many goroutines at once.
func (a *testAPI) request(method, path, body string) (int, map[string]any, error) {
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return rec.Code, nil, fmt.Errorf("%s %s: body is not JSON: %q", method, path, rec.Body.String())
	}
	return rec.Code, out, nil
}

// do is request for single-goroutine tests: it fails the test on a malformed response.
func (a *testAPI) do(method, path, body string) (int, map[string]any) {
	a.t.Helper()
	code, out, err := a.request(method, path, body)
	if err != nil {
		a.t.Fatal(err)
	}
	return code, out
}

var opCounter atomic.Int64

// newOpID returns an operation id nobody has used before, i.e. a brand-new request
// rather than a retry.
func newOpID() string {
	return fmt.Sprintf("op-%d-%d", time.Now().UnixNano(), opCounter.Add(1))
}

func reserveBody(opID, sku string) string {
	return fmt.Sprintf(`{"operation_id":%q,"sku":%q}`, opID, sku)
}

func finishBody(opID, reservationID string) string {
	return fmt.Sprintf(`{"operation_id":%q,"reservation_id":%q}`, opID, reservationID)
}

func (a *testAPI) reserve() string {
	a.t.Helper()
	code, body := a.do("POST", "/reserve", reserveBody(newOpID(), a.sku))
	if code != http.StatusCreated {
		a.t.Fatalf("reserve: status %d, body %v", code, body)
	}
	return body["reservation_id"].(string)
}

func (a *testAPI) finish(action, reservationID string) (int, map[string]any) {
	a.t.Helper()
	return a.do("POST", "/"+action, finishBody(newOpID(), reservationID))
}

// wantInventory checks GET /inventory and then that every invariant holds for the SKU.
func (a *testAPI) wantInventory(available, reserved, sold int) {
	a.t.Helper()
	code, body := a.do("GET", "/inventory/"+a.sku, "")
	if code != http.StatusOK {
		a.t.Fatalf("inventory: status %d, body %v", code, body)
	}
	got := [3]int{int(body["available"].(float64)), int(body["reserved"].(float64)), int(body["sold"].(float64))}
	if want := [3]int{available, reserved, sold}; got != want {
		a.t.Errorf("available/reserved/sold = %v, want %v", got, want)
	}

	ctx := context.Background()
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		a.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	vs, _, err := invariant.Check(ctx, tx)
	if err != nil {
		a.t.Fatal(err)
	}
	for _, v := range vs {
		if v.SKU == a.sku {
			a.t.Errorf("invariant violated: %s: %s", v.Kind, v.Detail)
		}
	}
}

func wantStatus(t *testing.T, what string, got, want int, body map[string]any) {
	t.Helper()
	if got != want {
		t.Errorf("%s: status %d, want %d (body %v)", what, got, want, body)
	}
}

func wantError(t *testing.T, what string, code, wantCode int, body map[string]any, wantErr string) {
	t.Helper()
	if code != wantCode || body["error"] != wantErr {
		t.Errorf("%s: got %d %v, want %d %q", what, code, body, wantCode, wantErr)
	}
}

// Happy path to a sale: reserving moves one unit available -> reserved,
// and confirming moves it reserved -> sold.
func TestReserveThenConfirm(t *testing.T) {
	a := newTestAPI(t, 10)
	a.wantInventory(10, 0, 0)

	id := a.reserve()
	a.wantInventory(9, 1, 0)

	code, body := a.finish("confirm", id)
	wantStatus(t, "confirm", code, http.StatusOK, body)
	if body["state"] != "CONFIRMED" || body["reservation_id"] != id {
		t.Errorf("confirm body = %v", body)
	}
	a.wantInventory(9, 0, 1)
}

// Happy path to a cancel: reserving holds a unit, abandoning puts it back,
// so the stock ends exactly where it started.
func TestReserveThenAbandon(t *testing.T) {
	a := newTestAPI(t, 10)
	id := a.reserve()
	a.wantInventory(9, 1, 0)

	code, body := a.finish("abandon", id)
	wantStatus(t, "abandon", code, http.StatusOK, body)
	if body["state"] != "ABANDONED" {
		t.Errorf("abandon body = %v", body)
	}
	a.wantInventory(10, 0, 0)
}

// The reserve response carries the SKU and a deadline about one hold window
// (5 minutes) in the future.
func TestReserveResponse(t *testing.T) {
	a := newTestAPI(t, 1)
	before := time.Now()
	code, body := a.do("POST", "/reserve", reserveBody(newOpID(), a.sku))
	wantStatus(t, "reserve", code, http.StatusCreated, body)
	if body["sku"] != a.sku {
		t.Errorf("sku = %v, want %s", body["sku"], a.sku)
	}
	deadline, err := time.Parse(time.RFC3339Nano, body["deadline"].(string))
	if err != nil {
		t.Fatal(err)
	}
	// Generous bounds: the deadline comes from the database clock, not this process.
	if d := deadline.Sub(before); d < defaultHoldWindow-time.Minute || d > defaultHoldWindow+time.Minute {
		t.Errorf("deadline is %v after the request, want about %v", d, defaultHoldWindow)
	}
}

// With 3 units, the first 3 reserves succeed and the 4th is refused with
// 409 out_of_stock without changing any counter.
func TestReserveUntilOutOfStock(t *testing.T) {
	a := newTestAPI(t, 3)
	for range 3 {
		a.reserve()
	}
	a.wantInventory(0, 3, 0)

	code, body := a.do("POST", "/reserve", reserveBody(newOpID(), a.sku))
	wantError(t, "4th reserve", code, http.StatusConflict, body, "out_of_stock")
	a.wantInventory(0, 3, 0)
}

// A unit that was abandoned really is back on the shelf: with stock 1,
// reserve -> abandon -> reserve succeeds the second time.
func TestAbandonedUnitCanBeReservedAgain(t *testing.T) {
	a := newTestAPI(t, 1)
	id := a.reserve()
	a.finish("abandon", id)
	a.reserve()
	a.wantInventory(0, 1, 0)
}

// Once a reservation is confirmed or abandoned it is final. Any second
// confirm/abandon gets 409 and must not move a unit again (the double-move bug).
func TestFinishedReservationCannotBeFinishedAgain(t *testing.T) {
	cases := []struct{ first, second string }{
		{"confirm", "confirm"},
		{"confirm", "abandon"},
		{"abandon", "abandon"},
		{"abandon", "confirm"},
	}
	for _, tc := range cases {
		t.Run(tc.first+" then "+tc.second, func(t *testing.T) {
			a := newTestAPI(t, 10)
			id := a.reserve()
			code, body := a.finish(tc.first, id)
			wantStatus(t, tc.first, code, http.StatusOK, body)

			code, body = a.finish(tc.second, id)
			wantError(t, "second "+tc.second, code, http.StatusConflict, body, "reservation_not_reserved")

			if tc.first == "confirm" {
				a.wantInventory(9, 0, 1)
			} else {
				a.wantInventory(10, 0, 0)
			}
		})
	}
}

// Unknown SKUs and unknown reservation ids get 404, and nothing changes.
func TestNotFound(t *testing.T) {
	a := newTestAPI(t, 1)
	const unknownID = "00000000-0000-0000-0000-000000000000"
	// These operations target things that do not exist, so the per-SKU cleanup misses them.
	t.Cleanup(func() {
		a.pool.Exec(context.Background(),
			`DELETE FROM operations WHERE request IN ('NO-SUCH-SKU', $1)`, unknownID)
	})

	code, body := a.do("POST", "/reserve", reserveBody(newOpID(), "NO-SUCH-SKU"))
	wantError(t, "reserve", code, http.StatusNotFound, body, "sku_not_found")

	code, body = a.do("GET", "/inventory/NO-SUCH-SKU", "")
	wantError(t, "inventory", code, http.StatusNotFound, body, "sku_not_found")

	for _, action := range []string{"confirm", "abandon"} {
		code, body = a.finish(action, unknownID)
		wantError(t, action, code, http.StatusNotFound, body, "reservation_not_found")
	}
	a.wantInventory(1, 0, 0)
}

// Broken JSON, missing fields (including a missing operation_id), and ids that
// are not UUIDs get 400,
// and nothing changes.
func TestInvalidRequest(t *testing.T) {
	a := newTestAPI(t, 1)
	requests := []struct{ path, body string }{
		{"/reserve", `not json`},
		{"/reserve", `{}`},
		{"/reserve", `{"sku":"` + a.sku + `"}`}, // no operation_id
		{"/confirm", `{}`},
		{"/confirm", `{"reservation_id":"00000000-0000-0000-0000-000000000000"}`}, // no operation_id
		{"/confirm", `{"operation_id":"x","reservation_id":"not-a-uuid"}`},
		{"/abandon", `{"operation_id":"x","reservation_id":"not-a-uuid"}`},
	}
	for _, r := range requests {
		code, body := a.do("POST", r.path, r.body)
		wantError(t, r.path+" "+r.body, code, http.StatusBadRequest, body, "invalid_request")
	}
	a.wantInventory(1, 0, 0)
}
