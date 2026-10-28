package service

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string
var ErrLeaseLost = errors.New("lease no longer owned")
var ErrDependency = errors.New("capture has not arrived")
var ErrConflict = errors.New("posting disagrees with previously recorded money")

type Store struct {
	DB      *pgxpool.Pool
	Version string
}

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(821497)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, schema); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) Enqueue(ctx context.Context, e Event, raw []byte) (bool, error) {
	tag, err := s.DB.Exec(ctx, `INSERT INTO inbox(event_id,payload) VALUES($1,$2) ON CONFLICT DO NOTHING`, e.ID, raw)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 0, nil
}

type Lease struct {
	EventID, Token string
	Raw            []byte
	Attempts       int
}

func (s *Store) Claim(ctx context.Context, ttl time.Duration) (*Lease, error) {
	if ttl <= 0 {
		return nil, errors.New("lease duration must be positive")
	}
	// Workers that die on their last attempt must also reach a terminal state.
	_, err := s.DB.Exec(ctx, `UPDATE inbox SET state='dead',lease_token=NULL,lease_until=NULL,
 last_error='attempt limit reached' WHERE attempts>=12 AND
 (state='queued' OR (state='processing' AND lease_until<clock_timestamp()))`)
	if err != nil {
		return nil, err
	}
	tokenBytes := make([]byte, 16)
	if _, err = rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	l := &Lease{Token: hex.EncodeToString(tokenBytes)}
	err = s.DB.QueryRow(ctx, `WITH next AS (
 SELECT event_id FROM inbox WHERE attempts<12 AND
 ((state='queued' AND available_at<=clock_timestamp()) OR
 (state='processing' AND lease_until<clock_timestamp()))
 ORDER BY available_at,event_id FOR UPDATE SKIP LOCKED LIMIT 1)
 UPDATE inbox i SET state='processing',attempts=attempts+1,lease_token=$1,
 lease_until=clock_timestamp()+$2*interval '1 millisecond'
 FROM next WHERE i.event_id=next.event_id RETURNING i.event_id,i.payload,i.attempts`, l.Token, ttl.Milliseconds()).Scan(&l.EventID, &l.Raw, &l.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return l, err
}
func (s *Store) Process(ctx context.Context, l *Lease) error { return s.process(ctx, l, nil) }

// beforeCommit is a test seam for a failure or process death after all writes.
func (s *Store) process(ctx context.Context, l *Lease, beforeCommit func() error) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM inbox WHERE event_id=$1 AND state='processing'
 AND lease_token=$2 AND lease_until>clock_timestamp() FOR UPDATE`, l.EventID, l.Token).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, l.Raw) {
		return errors.New("lease payload mismatch")
	}
	e, err := Decode(raw, s.Version)
	if err != nil {
		return err
	}
	p, err := Normalize(e)
	if err != nil {
		return err
	}
	if p != nil {
		if err = post(ctx, tx, *p, e.ID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE inbox SET state='done',lease_until=NULL,lease_token=NULL,last_error=NULL WHERE event_id=$1`, e.ID); err != nil {
		return err
	}
	if beforeCommit != nil {
		if err = beforeCommit(); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func post(ctx context.Context, tx pgx.Tx, p Posting, eventID string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Kind == "capture" {
		if _, err := tx.Exec(ctx, `INSERT INTO captures(charge_id,amount,currency) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, p.ChargeID, p.Amount, p.Currency); err != nil {
			return err
		}
	}
	var captured, refunded int64
	var currency string
	err := tx.QueryRow(ctx, `SELECT amount,refunded,currency FROM captures WHERE charge_id=$1 FOR UPDATE`, p.ChargeID).Scan(&captured, &refunded, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDependency
	}
	if err != nil {
		return err
	}
	if currency != p.Currency || (p.Kind == "capture" && captured != p.Amount) {
		return ErrConflict
	}
	var old Posting
	err = tx.QueryRow(ctx, `SELECT posting_key,kind,object_id,charge_id,amount,currency,provider_created FROM journal WHERE posting_key=$1`, p.Key).Scan(&old.Key, &old.Kind, &old.ObjectID, &old.ChargeID, &old.Amount, &old.Currency, &old.Created)
	if err == nil {
		if old != p {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	debit, credit := "stripe_clearing", "merchant_payable"
	if p.Kind == "refund" {
		if p.Amount > captured-refunded {
			return fmt.Errorf("%w: refunds exceed capture", ErrConflict)
		}
		if _, err = tx.Exec(ctx, `UPDATE captures SET refunded=refunded+$2 WHERE charge_id=$1`, p.ChargeID, p.Amount); err != nil {
			return err
		}
		debit, credit = credit, debit
	}
	_, err = tx.Exec(ctx, `INSERT INTO journal(posting_key,kind,object_id,charge_id,amount,currency,provider_created,debit_account,credit_account,source_event)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, p.Key, p.Kind, p.ObjectID, p.ChargeID, p.Amount, p.Currency, p.Created, debit, credit, eventID)
	return err
}
func (s *Store) Fail(ctx context.Context, l *Lease, cause error) error {
	delay := time.Second * time.Duration(1<<min(l.Attempts, 6))
	_, err := s.DB.Exec(ctx, `UPDATE inbox SET state=CASE WHEN attempts>=12 THEN 'dead' ELSE 'queued' END,
 lease_token=NULL,lease_until=NULL,last_error=$3,available_at=clock_timestamp()+$4*interval '1 millisecond'
 WHERE event_id=$1 AND state='processing' AND lease_token=$2`, l.EventID, l.Token, cause.Error(), delay.Milliseconds())
	return err
}
