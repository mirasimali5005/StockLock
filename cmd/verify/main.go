// Command verify checks an adversarial k6 run against the database. It reads the
// events file the k6 script wrote (one JSON object per request) and the database,
// and reports every way in which what shoppers were told disagrees with what is
// actually stored. It exits 0 when everything agrees and 1 otherwise.
//
//	go run ./cmd/verify -events events.jsonl -sku ADV-SKU -stock 50
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"stocklock/internal/invariant"
)

const defaultDatabaseURL = "postgres://stocklock:stocklock@localhost:5432/stocklock"

// clockTolerance allows for skew between k6's clock and the database's clock
// (the same machine, but Postgres runs inside a VM).
const clockTolerance = 100 * time.Millisecond

type event struct {
	Kind          string `json:"kind"`
	Op            string `json:"op"`
	Status        int    `json:"status"`
	SentAt        int64  `json:"sent_at"` // ms since epoch, client clock
	ReservationID string `json:"reservation_id"`
	Body          string `json:"body"`
	Storm         int    `json:"storm"`
}

type reservationRow struct {
	sku      string
	state    string
	deadline time.Time
}

type operationRow struct {
	kind     string
	request  string
	status   int
	response string
}

type report struct {
	failures []string
	counts   map[string]int
}

func (r *report) fail(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *report) count(key string) { r.counts[key]++ }

func main() {
	eventsPath := flag.String("events", "", "k6 events file (JSON lines)")
	sku := flag.String("sku", "ADV-SKU", "the SKU the run used")
	stock := flag.Int("stock", 50, "the SKU's initial stock")
	flag.Parse()
	if *eventsPath == "" {
		fmt.Fprintln(os.Stderr, "verify: -events is required")
		os.Exit(2)
	}

	events, err := readEvents(*eventsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(2)
	}

	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = defaultDatabaseURL
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(2)
	}
	defer conn.Close(ctx)

	rep, err := verify(ctx, conn, events, *sku, *stock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(2)
	}

	keys := make([]string, 0, len(rep.counts))
	for k := range rep.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-40s %d\n", k, rep.counts[k])
	}
	if len(rep.failures) == 0 {
		fmt.Println("VERIFY OK: every answer shoppers received agrees with the database")
		return
	}
	shown := rep.failures
	if len(shown) > 20 {
		shown = shown[:20]
	}
	for _, f := range shown {
		fmt.Println("FAILURE:", f)
	}
	fmt.Printf("VERIFY FAIL: %d failure(s)\n", len(rep.failures))
	os.Exit(1)
}

func readEvents(path string) ([]event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue // k6's own log lines, if any
		}
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("bad event line %q: %w", line, err)
		}
		events = append(events, e)
	}
	return events, sc.Err()
}

func verify(ctx context.Context, conn *pgx.Conn, events []event, sku string, stock int) (*report, error) {
	rep := &report{counts: map[string]int{}}
	rep.counts["events"] = len(events)

	// 1. The invariants, exactly as the checker sees them.
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	violations, _, err := invariant.Check(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, v := range violations {
		if v.SKU == sku {
			rep.fail("invariant %s: %s", v.Kind, v.Detail)
		}
	}

	// Load the SKU's reservations and every operation the run created.
	reservations := map[string]reservationRow{}
	rows, err := tx.Query(ctx,
		`SELECT reservation_id::text, sku, state, deadline FROM reservations WHERE sku = $1`, sku)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var r reservationRow
		if err := rows.Scan(&id, &r.sku, &r.state, &r.deadline); err != nil {
			return nil, err
		}
		reservations[id] = r
	}
	rows.Close()
	rep.counts["reservation rows"] = len(reservations)

	operations := map[string]operationRow{}
	rows, err = tx.Query(ctx, `SELECT operation_id, operation_type, request, status_code, response::text FROM operations`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var o operationRow
		if err := rows.Scan(&id, &o.kind, &o.request, &o.status, &o.response); err != nil {
			return nil, err
		}
		operations[id] = o
	}
	rows.Close()
	rep.counts["operation rows"] = len(operations)

	var available, reserved, sold int
	if err := tx.QueryRow(ctx, `SELECT available, reserved, sold FROM stock WHERE sku = $1`, sku).
		Scan(&available, &reserved, &sold); err != nil {
		return nil, err
	}
	rep.counts["final available"] = available
	rep.counts["final reserved"] = reserved
	rep.counts["final sold"] = sold
	if available+reserved+sold != stock {
		rep.fail("final counters %d/%d/%d do not sum to the initial stock %d", available, reserved, sold, stock)
	}

	// 2. Every answer a shopper received must match a row.
	byOp := map[string][]event{}
	for _, e := range events {
		byOp[e.Op] = append(byOp[e.Op], e)
		rep.count(fmt.Sprintf("%s -> %d", e.Kind, e.Status))
	}
	successfulReserveOps := map[string]string{} // op -> reservation id
	for _, e := range events {
		switch {
		case e.Status == 0:
			// Never reached the API (connection reset at the client). Nothing to check.
		case e.Kind == "reserve" && e.Status == 201:
			r, ok := reservations[e.ReservationID]
			if !ok {
				rep.fail("reserve %s got 201 with reservation %s, which does not exist", e.Op, e.ReservationID)
				continue
			}
			if r.sku != sku {
				rep.fail("reserve %s got reservation %s for the wrong SKU %s", e.Op, e.ReservationID, r.sku)
			}
			successfulReserveOps[e.Op] = e.ReservationID
		case e.Kind == "confirm" && e.Status == 200:
			checkFinished(rep, reservations, e, "CONFIRMED")
		case e.Kind == "abandon" && e.Status == 200:
			checkFinished(rep, reservations, e, "ABANDONED")
		case (e.Kind == "confirm" || e.Kind == "abandon") && e.Status == 409:
			// Told no. The row must not show the action as having happened.
			var body struct {
				Error string `json:"error"`
			}
			json.Unmarshal([]byte(e.Body), &body)
			rid := requestOf(operations, e.Op)
			if r, ok := reservations[rid]; ok && body.Error == "reservation_expired" &&
				(r.state == "CONFIRMED" || r.state == "ABANDONED") {
				rep.fail("%s %s was told expired but reservation %s is %s", e.Kind, e.Op, rid, r.state)
			}
		}
	}

	// 3. One unit per successful reserve: distinct operation ids got distinct reservations,
	//    and the number of successful reserve operations equals the reservation rows.
	seen := map[string]string{}
	for op, rid := range successfulReserveOps {
		if other, dup := seen[rid]; dup {
			rep.fail("reservation %s was handed to two different operations, %s and %s", rid, other, op)
		}
		seen[rid] = op
	}
	rep.counts["successful reserve operations"] = len(successfulReserveOps)
	if len(successfulReserveOps) != len(reservations) {
		rep.fail("%d operations were told 201 but %d reservation rows exist",
			len(successfulReserveOps), len(reservations))
	}

	// 4. Retries: every copy of an operation got the same answer, and one row exists.
	for op, es := range byOp {
		if len(es) < 2 {
			continue
		}
		rep.count("operations sent more than once")
		first := firstServed(es)
		if first == nil {
			continue
		}
		for _, e := range es {
			if e.Status == 0 {
				continue
			}
			if e.Status != first.Status || !sameJSON(e.Body, first.Body) {
				rep.fail("operation %s got different answers: %d %s vs %d %s", op, first.Status, first.Body, e.Status, e.Body)
				break
			}
		}
		if o, ok := operations[op]; !ok {
			rep.fail("operation %s was answered %d but has no operations row", op, first.Status)
		} else if o.status != first.Status {
			rep.fail("operation %s was answered %d but its row says %d", op, first.Status, o.status)
		}
	}

	// 5. Deadlines: nobody finished a reservation after its deadline. The client's send
	//    time is compared with the row's deadline, with tolerance for clock skew; a request
	//    sent after the deadline cannot have been decided before it.
	for _, e := range events {
		if (e.Kind != "confirm" && e.Kind != "abandon") || e.Status != 200 {
			continue
		}
		rid := requestOf(operations, e.Op)
		r, ok := reservations[rid]
		if !ok {
			continue // already reported above
		}
		sent := time.UnixMilli(e.SentAt)
		if sent.After(r.deadline.Add(clockTolerance)) {
			rep.fail("%s %s was sent %v after reservation %s's deadline and still succeeded",
				e.Kind, e.Op, sent.Sub(r.deadline).Round(time.Millisecond), rid)
		}
	}

	// 6. After the final sweep, no hold is still sitting past its deadline.
	var overdue int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM reservations WHERE sku = $1 AND state = 'RESERVED' AND deadline <= clock_timestamp()`,
		sku).Scan(&overdue); err != nil {
		return nil, err
	}
	if overdue != 0 {
		rep.fail("%d RESERVED rows are past their deadline after the final sweep", overdue)
	}

	for _, r := range reservations {
		rep.count("reservations ending " + r.state)
	}
	return rep, nil
}

// checkFinished verifies that a 200 confirm/abandon answer matches the row's state.
func checkFinished(rep *report, reservations map[string]reservationRow, e event, want string) {
	if e.ReservationID == "" {
		rep.fail("%s %s got 200 without a reservation_id in the body: %s", e.Kind, e.Op, e.Body)
		return
	}
	r, ok := reservations[e.ReservationID]
	if !ok {
		rep.fail("%s %s got 200 for reservation %s, which does not exist", e.Kind, e.Op, e.ReservationID)
		return
	}
	if r.state != want {
		rep.fail("%s %s got 200 but reservation %s is %s, not %s", e.Kind, e.Op, e.ReservationID, r.state, want)
	}
}

// requestOf returns what an operation targeted, from its stored row.
func requestOf(operations map[string]operationRow, op string) string {
	return operations[op].request
}

// firstServed returns the first copy of an operation that actually reached the API.
func firstServed(es []event) *event {
	for i := range es {
		if es[i].Status != 0 {
			return &es[i]
		}
	}
	return nil
}

// sameJSON compares two bodies as JSON values, ignoring key order and whitespace.
func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return a == b
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}
