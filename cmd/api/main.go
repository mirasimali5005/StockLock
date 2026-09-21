// Command api serves the inventory reservation HTTP API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultDatabaseURL = "postgres://stocklock:stocklock@localhost:5432/stocklock"
	listenAddr         = ":8080"

	// holdWindow is written into each reservation's deadline. Nothing enforces the deadline yet.
	holdWindow = 5 * time.Minute
)

var (
	errSKUNotFound         = errors.New("sku_not_found")
	errOutOfStock          = errors.New("out_of_stock")
	errReservationNotFound = errors.New("reservation_not_found")
	errNotReserved         = errors.New("reservation_not_reserved")
)

type server struct {
	pool *pgxpool.Pool
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reserve", s.handleReserve)
	mux.HandleFunc("POST /confirm", s.handleConfirm)
	mux.HandleFunc("POST /abandon", s.handleAbandon)
	mux.HandleFunc("GET /inventory/{sku}", s.handleInventory)
	return mux
}

type reservation struct {
	ReservationID string    `json:"reservation_id"`
	SKU           string    `json:"sku"`
	Deadline      time.Time `json:"deadline"`
}

// reserve is deliberately the straightforward version: lock the stock row, read it,
// decide in application code, then write. Each step is its own round trip, all made
// while the row lock is held.
func (s *server) reserve(ctx context.Context, sku string) (reservation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return reservation{}, err
	}
	defer tx.Rollback(ctx)

	var available int
	err = tx.QueryRow(ctx, `SELECT available FROM stock WHERE sku = $1 FOR UPDATE`, sku).Scan(&available)
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
	if err != nil {
		return reservation{}, err
	}
	return r, tx.Commit(ctx)
}

// Where a held unit goes when its reservation is finished.
const (
	confirmStockSQL = `UPDATE stock SET reserved = reserved - 1, sold = sold + 1 WHERE sku = $1`
	abandonStockSQL = `UPDATE stock SET reserved = reserved - 1, available = available + 1 WHERE sku = $1`
)

// finish moves a RESERVED reservation to newState and moves its unit with stockSQL.
// Lock order is always reservation row, then stock row.
func (s *server) finish(ctx context.Context, id pgtype.UUID, newState, stockSQL string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var sku, state string
	err = tx.QueryRow(ctx,
		`SELECT sku, state FROM reservations WHERE reservation_id = $1 FOR UPDATE`, id).Scan(&sku, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return errReservationNotFound
	}
	if err != nil {
		return err
	}
	if state != "RESERVED" {
		return errNotReserved
	}

	if _, err := tx.Exec(ctx, stockSQL, sku); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE reservations SET state = $2 WHERE reservation_id = $1`, id, newState); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *server) handleReserve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SKU string `json:"sku"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SKU == "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	res, err := s.reserve(r.Context(), req.SKU)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	s.handleFinish(w, r, "CONFIRMED", confirmStockSQL)
}

func (s *server) handleAbandon(w http.ResponseWriter, r *http.Request) {
	s.handleFinish(w, r, "ABANDONED", abandonStockSQL)
}

func (s *server) handleFinish(w http.ResponseWriter, r *http.Request, newState, stockSQL string) {
	var req struct {
		ReservationID string `json:"reservation_id"`
	}
	var id pgtype.UUID
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || id.Scan(req.ReservationID) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := s.finish(r.Context(), id, newState, stockSQL); err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"reservation_id": req.ReservationID, "state": newState})
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
	if errors.Is(err, pgx.ErrNoRows) {
		err = errSKUNotFound
	}
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

func writeFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSKUNotFound), errors.Is(err, errReservationNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errOutOfStock), errors.Is(err, errNotReserved):
		writeError(w, http.StatusConflict, err.Error())
	default:
		log.Printf("internal error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
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
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(context.Background()); err != nil {
		log.Fatal(err)
	}

	log.Printf("listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, (&server{pool: pool}).routes()))
}
