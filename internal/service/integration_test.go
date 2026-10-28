package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("REQUIRE_DB_TESTS") == "1" {
			t.Fatal("TEST_DATABASE_URL is required")
		}
		t.Skip("set TEST_DATABASE_URL for PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 16
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	s := &Store{DB: db, Version: APIVersion}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}
func enqueue(t *testing.T, s *Store, id, kind string, obj any) {
	t.Helper()
	raw := eventRaw(id, kind, obj)
	e, err := Decode(raw, APIVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Enqueue(context.Background(), e, raw); err != nil {
		t.Fatal(err)
	}
}
func nextLease(t *testing.T, s *Store) *Lease {
	t.Helper()
	l, err := s.Claim(context.Background(), 30*time.Second)
	if err != nil || l == nil {
		t.Fatalf("claim %+v %v", l, err)
	}
	return l
}
func runOne(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Process(context.Background(), nextLease(t, s)); err != nil {
		t.Fatal(err)
	}
}
func assertLedger(t *testing.T, s *Store, postings int, net int64) {
	t.Helper()
	var count int
	var balance int64
	if err := s.DB.QueryRow(context.Background(), `SELECT COUNT(*) FROM journal`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != postings {
		t.Fatalf("got %d postings want %d", count, postings)
	}
	if err := s.DB.QueryRow(context.Background(), `SELECT COALESCE(SUM(signed_amount),0)::bigint FROM ledger_entries`).Scan(&balance); err != nil || balance != 0 {
		t.Fatalf("unbalanced ledger %d %v", balance, err)
	}
	if err := s.DB.QueryRow(context.Background(), `SELECT COALESCE(SUM(signed_amount),0)::bigint FROM ledger_entries WHERE account='stripe_clearing'`).Scan(&balance); err != nil || balance != net {
		t.Fatalf("net %d want %d %v", balance, net, err)
	}
	var broken int
	if err := s.DB.QueryRow(context.Background(), `SELECT COUNT(*) FROM (SELECT posting_key FROM ledger_entries GROUP BY posting_key HAVING COUNT(*)<>2 OR SUM(signed_amount)<>0) b`).Scan(&broken); err != nil || broken != 0 {
		t.Fatalf("broken journals %d %v", broken, err)
	}
}
func TestHTTPDurableInboxAndReplay(t *testing.T) {
	s := testStore(t)
	handler := API{s, "whsec_test", "0123456789abcdef"}.Handler()
	raw := eventRaw("evt_a", "charge.succeeded", charge("ch_a", 1000))
	send := func(sig string) int {
		req := httptest.NewRequest("POST", "/webhooks/stripe", bytes.NewReader(raw))
		req.Header.Set("Stripe-Signature", sig)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}
	if code := send("invalid"); code != 400 {
		t.Fatal(code)
	}
	assertLedger(t, s, 0, 0)
	sig := signature(raw, "whsec_test", time.Now())
	if code := send(sig); code != 202 {
		t.Fatal(code)
	}
	assertLedger(t, s, 0, 0)
	for i := 0; i < 20; i++ {
		if code := send(sig); code != 202 {
			t.Fatal(code)
		}
	}
	runOne(t, s)
	assertLedger(t, s, 1, 1000)
	// Another event type and ID can describe the very same capture.
	enqueue(t, s, "evt_b", "charge.captured", charge("ch_a", 1000))
	runOne(t, s)
	assertLedger(t, s, 1, 1000)
	if code := send(sig); code != 202 {
		t.Fatal(code)
	}
	assertLedger(t, s, 1, 1000)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/v1/balances", nil))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	req := httptest.NewRequest("GET", "/v1/balances", nil)
	req.Header.Set("Authorization", "Bearer 0123456789abcdef")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestRefundBeforeCapture(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	enqueue(t, s, "evt_refund", "refund.updated", refund("re_a", "ch_a", 200))
	l := nextLease(t, s)
	err := s.Process(ctx, l)
	if !errors.Is(err, ErrDependency) {
		t.Fatal(err)
	}
	if err = s.Fail(ctx, l, err); err != nil {
		t.Fatal(err)
	}
	assertLedger(t, s, 0, 0)
	enqueue(t, s, "evt_capture", "charge.captured", charge("ch_a", 1000))
	runOne(t, s)
	s.DB.Exec(ctx, `UPDATE inbox SET available_at=clock_timestamp() WHERE state='queued'`)
	runOne(t, s)
	enqueue(t, s, "evt_refund_pending", "refund.created", func() Refund { r := refund("re_a", "ch_a", 200); r.Status = "pending"; return r }())
	runOne(t, s)
	assertLedger(t, s, 2, 800)
}
func TestConcurrentWorkersAndRefundLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	enqueue(t, s, "evt_capture", "charge.captured", charge("ch_a", 10000))
	runOne(t, s)
	for i := 0; i < 40; i++ {
		for repeat := 0; repeat < 3; repeat++ {
			enqueue(t, s, fmt.Sprintf("evt_%02d_%d", i, repeat), "refund.updated", refund(fmt.Sprintf("re_%02d", i), "ch_a", 100))
		}
	}
	var wg sync.WaitGroup
	failures := make(chan error, 128)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				l, err := s.Claim(ctx, 30*time.Second)
				if err != nil {
					failures <- err
					return
				}
				if l == nil {
					return
				}
				if err = s.Process(ctx, l); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	assertLedger(t, s, 41, 6000)
	enqueue(t, s, "evt_over_a", "refund.updated", refund("re_over_a", "ch_a", 4000))
	enqueue(t, s, "evt_over_b", "refund.updated", refund("re_over_b", "ch_a", 4000))
	a, b := nextLease(t, s), nextLease(t, s)
	outcomes := make(chan error, 2)
	go func() { outcomes <- s.Process(ctx, a) }()
	go func() { outcomes <- s.Process(ctx, b) }()
	successes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-outcomes
		if err == nil {
			successes++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal(successes, conflicts)
	}
	assertLedger(t, s, 42, 2000)
}
func TestLeaseFencingAndAtomicRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	enqueue(t, s, "evt_a", "charge.captured", charge("ch_a", 1000))
	old := nextLease(t, s)
	s.DB.Exec(ctx, `UPDATE inbox SET lease_until=clock_timestamp()-interval '1 second'`)
	fresh := nextLease(t, s)
	if err := s.Process(ctx, old); !errors.Is(err, ErrLeaseLost) {
		t.Fatal(err)
	}
	injected := errors.New("injected precommit failure")
	if err := s.process(ctx, fresh, func() error { return injected }); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	assertLedger(t, s, 0, 0)
	var captures int
	s.DB.QueryRow(ctx, `SELECT COUNT(*) FROM captures`).Scan(&captures)
	if captures != 0 {
		t.Fatal("projection escaped rollback")
	}
	if err := s.Process(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	assertLedger(t, s, 1, 1000)
	for _, sql := range []string{`UPDATE journal SET amount=1`, `DELETE FROM journal`, `TRUNCATE journal`} {
		if _, err := s.DB.Exec(ctx, sql); err == nil {
			t.Fatalf("mutation allowed: %s", sql)
		}
	}
}
func TestCrashHelper(t *testing.T) {
	if os.Getenv("PAYMENTS_CRASH_HELPER") != "1" {
		t.Skip("subprocess only")
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		os.Exit(80)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = os.Getenv("PAYMENTS_TEST_SCHEMA")
	db, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		os.Exit(81)
	}
	raw, err := base64.StdEncoding.DecodeString(os.Getenv("PAYMENTS_TEST_LEASE"))
	if err != nil {
		os.Exit(82)
	}
	var lease Lease
	if json.Unmarshal(raw, &lease) != nil {
		os.Exit(83)
	}
	s := &Store{db, APIVersion}
	_ = s.process(context.Background(), &lease, func() error { os.Exit(77); return nil })
	os.Exit(84)
}
func TestProcessDeathBeforeCommit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	enqueue(t, s, "evt_crash", "charge.captured", charge("ch_crash", 1000))
	lease := nextLease(t, s)
	raw, _ := json.Marshal(lease)
	var schema string
	if err := s.DB.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$")
	cmd.Env = append(os.Environ(), "PAYMENTS_CRASH_HELPER=1", "PAYMENTS_TEST_SCHEMA="+schema, "PAYMENTS_TEST_LEASE="+base64.StdEncoding.EncodeToString(raw))
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 77 {
		t.Fatalf("child failed: %v %s", err, output)
	}
	assertLedger(t, s, 0, 0)
	if _, err = s.DB.Exec(ctx, `UPDATE inbox SET lease_until=clock_timestamp()-interval '1 second' WHERE event_id=$1`, lease.EventID); err != nil {
		t.Fatal(err)
	}
	runOne(t, s)
	assertLedger(t, s, 1, 1000)
}

type staticProvider struct{ SnapshotValue Snapshot }

func (p staticProvider) Snapshot(context.Context, time.Time) (Snapshot, error) {
	return p.SnapshotValue, nil
}
func TestReconcilePersistsSeededOmissions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := charge("ch_a", 1000)
	r := refund("re_a", "ch_a", 100)
	enqueue(t, s, "evt_capture", "charge.captured", c)
	runOne(t, s)
	report, err := s.Reconcile(ctx, FixtureProvider{Snapshot{[]Charge{c}, []Refund{r}}}, time.Now())
	if err != nil || len(report.Issues) != 1 || report.Issues[0].Kind != "missing_posting" {
		t.Fatal(report, err)
	}
	if report.Source != "synthetic_fixture" {
		t.Fatal("fixture provenance missing")
	}
	var count int
	if err = s.DB.QueryRow(ctx, `SELECT COUNT(*) FROM reconciliation_runs`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestTerminalAttemptRecovery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	enqueue(t, s, "evt_terminal", "charge.captured", charge("ch_terminal", 1000))
	if _, err := s.DB.Exec(ctx, `UPDATE inbox SET attempts=11`); err != nil {
		t.Fatal(err)
	}
	l := nextLease(t, s)
	if err := s.Fail(ctx, l, errors.New("last attempt failed")); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.DB.QueryRow(ctx, `SELECT state FROM inbox`).Scan(&state); err != nil || state != "dead" {
		t.Fatal(state, err)
	}
	// Simulate a worker dying on its twelfth claim instead of calling Fail.
	if _, err := s.DB.Exec(ctx, `UPDATE inbox SET state='processing',lease_until=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if l, err := s.Claim(ctx, time.Second); err != nil || l != nil {
		t.Fatal(l, err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT state FROM inbox`).Scan(&state); err != nil || state != "dead" {
		t.Fatal(state, err)
	}
	assertLedger(t, s, 0, 0)
}
func TestRandomizedDeliveries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var deliveries [][]byte
	for i := 0; i < 30; i++ {
		ch := fmt.Sprintf("ch_%03d", i)
		for repeat := 0; repeat < 3; repeat++ {
			deliveries = append(deliveries, eventRaw(fmt.Sprintf("evt_c_%d_%d", i, repeat), "charge.captured", charge(ch, 1000)), eventRaw(fmt.Sprintf("evt_r_%d_%d", i, repeat), "refund.updated", refund(fmt.Sprintf("re_%03d", i), ch, 250)))
		}
	}
	rng := rand.New(rand.NewSource(42))
	rng.Shuffle(len(deliveries), func(i, j int) { deliveries[i], deliveries[j] = deliveries[j], deliveries[i] })
	for _, raw := range deliveries {
		e, err := Decode(raw, APIVersion)
		if err != nil {
			t.Fatal(err)
		}
		for repeat := 0; repeat < 2; repeat++ {
			if _, err = s.Enqueue(ctx, e, raw); err != nil {
				t.Fatal(err)
			}
		}
	}
	for pass := 0; pass < 4; pass++ {
		for {
			l, err := s.Claim(ctx, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if l == nil {
				break
			}
			if err = s.Process(ctx, l); err != nil {
				if !errors.Is(err, ErrDependency) {
					t.Fatal(err)
				}
				if err = s.Fail(ctx, l, err); err != nil {
					t.Fatal(err)
				}
			}
		}
		s.DB.Exec(ctx, `UPDATE inbox SET available_at=clock_timestamp() WHERE state='queued'`)
	}
	assertLedger(t, s, 60, 22500)
	var pending int
	if err := s.DB.QueryRow(ctx, `SELECT COUNT(*) FROM inbox WHERE state<>'done'`).Scan(&pending); err != nil || pending != 0 {
		t.Fatal(pending, err)
	}
}
