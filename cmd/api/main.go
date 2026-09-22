// Command api serves the inventory reservation HTTP API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"stocklock/internal/metrics"
)

const (
	defaultDatabaseURL = "postgres://stocklock:stocklock@localhost:5432/stocklock"
	defaultListenAddr  = ":8080"

	// defaultHoldWindow is how long a reservation stays valid unless HOLD_WINDOW says otherwise.
	defaultHoldWindow = 5 * time.Minute
)

// Definite answers. Each is a final, repeatable outcome of an operation, so it is
// stored with the operation and replayed to retries. Any other error is an internal
// failure: the transaction rolls back and the operation id stays unused.
var (
	errSKUNotFound         = errors.New("sku_not_found")
	errOutOfStock          = errors.New("out_of_stock")
	errReservationNotFound = errors.New("reservation_not_found")
	errNotReserved         = errors.New("reservation_not_reserved")
	errExpired             = errors.New("reservation_expired")
)

// errOperationReuse means an operation_id arrived again with a different request.
var errOperationReuse = errors.New("operation_id_reuse")

// definiteStatus maps a definite answer to its HTTP status.
func definiteStatus(err error) (int, bool) {
	switch {
	case errors.Is(err, errSKUNotFound), errors.Is(err, errReservationNotFound):
		return http.StatusNotFound, true
	case errors.Is(err, errOutOfStock), errors.Is(err, errNotReserved), errors.Is(err, errExpired):
		return http.StatusConflict, true
	}
	return 0, false
}

type server struct {
	pool       *pgxpool.Pool
	holdWindow time.Duration
	metrics    metrics.Registry
}

// Timer names, suffixed with the operation kind (e.g. "reserve.critical_section").
//
//	lock_wait:        the SELECT ... FOR UPDATE statement, i.e. queueing for the row lock
//	critical_section: row lock acquired -> commit returned, how long the row is held
//	transaction:      BEGIN -> commit returned
const (
	timerLockWait        = ".lock_wait"
	timerCriticalSection = ".critical_section"
	timerTransaction     = ".transaction"
)

// timing carries the moments a mutation records on its way through a transaction.
type timing struct {
	kind         string
	lockWait     time.Duration
	lockAcquired time.Time
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reserve", s.handleReserve)
	mux.HandleFunc("POST /confirm", s.handleConfirm)
	mux.HandleFunc("POST /abandon", s.handleAbandon)
	mux.HandleFunc("GET /inventory/{sku}", s.handleInventory)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("POST /stats/reset", s.handleStatsReset)
	return mux
}

// operation identifies one client request: a retry repeats all three fields.
type operation struct {
	id      string
	kind    string // RESERVE, CONFIRM, or ABANDON
	request string // the SKU for RESERVE, the reservation id otherwise
}

// runOnce executes mutate at most once per operation id and returns the HTTP status
// and JSON body to send. The operation row and the inventory change commit in one
// transaction, so neither can exist without the other.
//
// The id is claimed first with INSERT ... ON CONFLICT DO NOTHING. If another
// transaction is mid-flight with the same id, the insert waits for it: if that one
// commits we replay its stored answer, and if it rolls back we take over as the
// first attempt.
func (s *server) runOnce(ctx context.Context, op operation, successStatus int,
	mutate func(tx pgx.Tx, tm *timing) (any, error)) (int, []byte, error) {

	began := time.Now()
	tm := timing{kind: op.kind}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)

	claimed, err := tx.Exec(ctx,
		`INSERT INTO operations (operation_id, operation_type, request) VALUES ($1, $2, $3)
		 ON CONFLICT (operation_id) DO NOTHING`, op.id, op.kind, op.request)
	if err != nil {
		return 0, nil, err
	}
	if claimed.RowsAffected() == 0 {
		var kind, request string
		var status int
		var body []byte
		if err := tx.QueryRow(ctx,
			`SELECT operation_type, request, status_code, response FROM operations WHERE operation_id = $1`,
			op.id).Scan(&kind, &request, &status, &body); err != nil {
			return 0, nil, err
		}
		if kind != op.kind || request != op.request {
			return 0, nil, errOperationReuse
		}
		return status, body, nil
	}

	status := successStatus
	result, err := mutate(tx, &tm)
	if err != nil {
		var definite bool
		if status, definite = definiteStatus(err); !definite {
			return 0, nil, err
		}
		result = map[string]string{"error": err.Error()}
	}
	body, err := json.Marshal(result)
	if err != nil {
		return 0, nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE operations SET status_code = $2, response = $3 WHERE operation_id = $1`,
		op.id, status, body); err != nil {
		return 0, nil, err
	}
	err = tx.Commit(ctx)
	if err == nil && !tm.lockAcquired.IsZero() {
		done := time.Now()
		s.metrics.Record(tm.kind+timerLockWait, tm.lockWait)
		s.metrics.Record(tm.kind+timerCriticalSection, done.Sub(tm.lockAcquired))
		s.metrics.Record(tm.kind+timerTransaction, done.Sub(began))
	}
	return status, body, err
}

type reservation struct {
	ReservationID string    `json:"reservation_id"`
	SKU           string    `json:"sku"`
	Deadline      time.Time `json:"deadline"`
}

// reserve is deliberately the straightforward version: lock the stock row, read it,
// decide in application code, then write. Each step is its own round trip, all made
// while the row lock is held.
func reserve(ctx context.Context, tx pgx.Tx, sku string, holdWindow time.Duration, tm *timing) (reservation, error) {
	var available int
	lockStart := time.Now()
	err := tx.QueryRow(ctx, `SELECT available FROM stock WHERE sku = $1 FOR UPDATE`, sku).Scan(&available)
	tm.lockAcquired = time.Now()
	tm.lockWait = tm.lockAcquired.Sub(lockStart)
	if errors.Is(err, pgx.ErrNoRows) {
		return reservation{}, errSKUNotFound
	}
	if err != nil {
		return reservation{}, err
	}
	if available <= 0 {
		return reservation{}, errOutOfStock
	}

	if _, err := tx.Exec(ctx,
		`UPDATE stock SET available = available - 1, reserved = reserved + 1 WHERE sku = $1`, sku); err != nil {
		return reservation{}, err
	}

	r := reservation{SKU: sku}
	err = tx.QueryRow(ctx,
		`INSERT INTO reservations (sku, state, deadline)
		 VALUES ($1, 'RESERVED', now() + $2::interval)
		 RETURNING reservation_id::text, deadline`,
		sku, holdWindow).Scan(&r.ReservationID, &r.Deadline)
	return r, err
}

// Where a held unit goes when its reservation is finished.
const (
	confirmStockSQL = `UPDATE stock SET reserved = reserved - 1, sold = sold + 1 WHERE sku = $1`
	abandonStockSQL = `UPDATE stock SET reserved = reserved - 1, available = available + 1 WHERE sku = $1`
)

// finish moves a RESERVED, unexpired reservation to newState and moves its unit with
// stockSQL. Lock order is always reservation row, then stock row.
//
// The deadline is checked with clock_timestamp(), the real current time, and only
// after the row lock is held. now() would be wrong here: it is frozen at transaction
// start, so a request that started before the deadline but waited on the lock past
// it would still be allowed through.
func finish(ctx context.Context, tx pgx.Tx, id pgtype.UUID, newState, stockSQL string, tm *timing) error {
	var sku, state string
	lockStart := time.Now()
	err := tx.QueryRow(ctx,
		`SELECT sku, state FROM reservations WHERE reservation_id = $1 FOR UPDATE`, id).Scan(&sku, &state)
	tm.lockAcquired = time.Now()
	tm.lockWait = tm.lockAcquired.Sub(lockStart)
	if errors.Is(err, pgx.ErrNoRows) {
		return errReservationNotFound
	}
	if err != nil {
		return err
	}
	switch state {
	case "RESERVED":
	case "EXPIRED":
		// The sweeper got here first. Same answer as arriving late ourselves.
		return errExpired
	default:
		return errNotReserved
	}

	updated, err := tx.Exec(ctx,
		`UPDATE reservations SET state = $2
		 WHERE reservation_id = $1 AND deadline > clock_timestamp()`, id, newState)
	if err != nil {
		return err
	}
	if updated.RowsAffected() == 0 {
		return errExpired
	}
	_, err = tx.Exec(ctx, stockSQL, sku)
	return err
}

func (s *server) handleReserve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OperationID string `json:"operation_id"`
		SKU         string `json:"sku"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OperationID == "" || req.SKU == "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	op := operation{id: req.OperationID, kind: "RESERVE", request: req.SKU}
	status, body, err := s.runOnce(r.Context(), op, http.StatusCreated, func(tx pgx.Tx, tm *timing) (any, error) {
		return reserve(r.Context(), tx, req.SKU, s.holdWindow, tm)
	})
	writeOutcome(w, status, body, err)
}

func (s *server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	s.handleFinish(w, r, "CONFIRM", "CONFIRMED", confirmStockSQL)
}

func (s *server) handleAbandon(w http.ResponseWriter, r *http.Request) {
	s.handleFinish(w, r, "ABANDON", "ABANDONED", abandonStockSQL)
}

func (s *server) handleFinish(w http.ResponseWriter, r *http.Request, kind, newState, stockSQL string) {
	var req struct {
		OperationID   string `json:"operation_id"`
		ReservationID string `json:"reservation_id"`
	}
	var id pgtype.UUID
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OperationID == "" ||
		id.Scan(req.ReservationID) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	// The canonical text form, so differently written copies of one UUID compare equal.
	reservationID, _ := id.Value()
	op := operation{id: req.OperationID, kind: kind, request: reservationID.(string)}
	status, body, err := s.runOnce(r.Context(), op, http.StatusOK, func(tx pgx.Tx, tm *timing) (any, error) {
		if err := finish(r.Context(), tx, id, newState, stockSQL, tm); err != nil {
			return nil, err
		}
		return map[string]string{"reservation_id": op.request, "state": newState}, nil
	})
	writeOutcome(w, status, body, err)
}

func (s *server) handleInventory(w http.ResponseWriter, r *http.Request) {
	inv := struct {
		SKU       string `json:"sku"`
		Available int    `json:"available"`
		Reserved  int    `json:"reserved"`
		Sold      int    `json:"sold"`
	}{SKU: r.PathValue("sku")}

	err := s.pool.QueryRow(r.Context(),
		`SELECT available, reserved, sold FROM stock WHERE sku = $1`, inv.SKU).
		Scan(&inv.Available, &inv.Reserved, &inv.Sold)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, errSKUNotFound.Error())
	case err != nil:
		writeOutcome(w, 0, nil, err)
	default:
		writeJSON(w, http.StatusOK, inv)
	}
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.metrics.Summaries())
}

func (s *server) handleStatsReset(w http.ResponseWriter, r *http.Request) {
	s.metrics.Reset()
	w.WriteHeader(http.StatusNoContent)
}

// writeOutcome sends what runOnce decided: a first or replayed answer, or a failure.
func writeOutcome(w http.ResponseWriter, status int, body []byte, err error) {
	switch {
	case errors.Is(err, errOperationReuse):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		log.Printf("internal error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
	}
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = defaultDatabaseURL
	}
	holdWindow := defaultHoldWindow
	if v := os.Getenv("HOLD_WINDOW"); v != "" {
		var err error
		if holdWindow, err = time.ParseDuration(v); err != nil || holdWindow <= 0 {
			log.Fatalf("HOLD_WINDOW %q: want a positive duration such as 5m or 2s", v)
		}
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		log.Fatal(err)
	}
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Fatalf("DB_MAX_CONNS %q: want a positive integer", v)
		}
		cfg.MaxConns = int32(n)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(context.Background()); err != nil {
		log.Fatal(err)
	}
	listenAddr := defaultListenAddr
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		listenAddr = v
	}

	log.Printf("listening on %s, hold window %v, pool max %d", listenAddr, holdWindow, cfg.MaxConns)
	log.Fatal(http.ListenAndServe(listenAddr, (&server{pool: pool, holdWindow: holdWindow}).routes()))
}
