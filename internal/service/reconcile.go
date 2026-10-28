package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Snapshot struct {
	Charges []Charge `json:"charges"`
	Refunds []Refund `json:"refunds"`
}

// FixtureProvider is an explicit offline demonstration source, never a Stripe API result.
type FixtureProvider struct{ Data Snapshot }

func (p FixtureProvider) Source() string { return "synthetic_fixture" }

func (p FixtureProvider) Snapshot(context.Context, time.Time) (Snapshot, error) { return p.Data, nil }

type Provider interface {
	Snapshot(context.Context, time.Time) (Snapshot, error)
}
type Stripe struct {
	Key, Version string
	Client       *http.Client
}

func (s Stripe) Source() string { return "stripe_test_api" }

func (s Stripe) Snapshot(ctx context.Context, cutoff time.Time) (Snapshot, error) {
	var out Snapshot
	if !strings.HasPrefix(s.Key, "sk_test_") && !strings.HasPrefix(s.Key, "rk_test_") {
		return out, errors.New("a test-mode Stripe API key is required")
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	version := s.Version
	if version == "" {
		version = APIVersion
	}
	for _, resource := range []string{"charges", "refunds"} {
		cursor := ""
		seen := map[string]bool{}
		for {
			q := url.Values{"limit": {"100"}, "created[lte]": {strconv.FormatInt(cutoff.Unix(), 10)}}
			if cursor != "" {
				q.Set("starting_after", cursor)
			}
			req, err := http.NewRequestWithContext(ctx, "GET", "https://api.stripe.com/v1/"+resource+"?"+q.Encode(), nil)
			if err != nil {
				return out, err
			}
			req.SetBasicAuth(s.Key, "")
			req.Header.Set("Stripe-Version", version)
			resp, err := client.Do(req)
			if err != nil {
				return out, err
			}
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			if readErr != nil {
				return out, readErr
			}
			if resp.StatusCode != 200 {
				return out, fmt.Errorf("Stripe %s returned HTTP %d", resource, resp.StatusCode)
			}
			var page struct {
				Object string            `json:"object"`
				Data   []json.RawMessage `json:"data"`
				More   bool              `json:"has_more"`
			}
			if err = json.Unmarshal(body, &page); err != nil {
				return out, err
			}
			if page.Object != "list" {
				return out, errors.New("unexpected Stripe list response")
			}
			for _, raw := range page.Data {
				var id struct {
					ID string `json:"id"`
				}
				if err = json.Unmarshal(raw, &id); err != nil {
					return out, err
				}
				if id.ID == "" || seen[id.ID] {
					return out, errors.New("invalid or repeated pagination object")
				}
				seen[id.ID] = true
				cursor = id.ID
				if resource == "charges" {
					var c Charge
					if err = json.Unmarshal(raw, &c); err != nil {
						return out, err
					}
					out.Charges = append(out.Charges, c)
				} else {
					var r Refund
					if err = json.Unmarshal(raw, &r); err != nil {
						return out, err
					}
					out.Refunds = append(out.Refunds, r)
				}
			}
			if !page.More {
				break
			}
			if len(page.Data) == 0 {
				return out, errors.New("empty pagination page with has_more=true")
			}
		}
	}
	return out, nil
}

type Issue struct {
	Kind     string   `json:"kind"`
	Key      string   `json:"key"`
	Expected *Posting `json:"expected,omitempty"`
	Actual   *Posting `json:"actual,omitempty"`
}
type Report struct {
	Source           string    `json:"source"`
	Cutoff           time.Time `json:"cutoff"`
	ProviderObjects  int       `json:"provider_objects"`
	ExpectedPostings int       `json:"expected_postings"`
	ActualPostings   int       `json:"actual_postings"`
	Issues           []Issue   `json:"issues"`
}

func Compare(snapshot Snapshot, actual []Posting, cutoff time.Time) (Report, error) {
	report := Report{Cutoff: cutoff, ActualPostings: len(actual), Issues: []Issue{}}
	expected := map[string]Posting{}
	observed := map[string]bool{}
	for _, c := range snapshot.Charges {
		if c.Created > cutoff.Unix() {
			continue
		}
		report.ProviderObjects++
		observed["capture:"+c.ID] = true
		// Non-card payments are outside this service's scope.
		if c.PaymentMethodDetails.Type != "card" {
			continue
		}
		p, err := chargePosting(c)
		if err != nil {
			return report, err
		}
		if p != nil {
			expected[p.Key] = *p
		}
	}
	for _, r := range snapshot.Refunds {
		if r.Created > cutoff.Unix() {
			continue
		}
		report.ProviderObjects++
		observed["refund:"+r.ID] = true
		p, err := refundPosting(r)
		if err != nil {
			return report, err
		}
		// A refund must refer to an in-scope captured card charge.
		if p != nil {
			if _, ok := expected["capture:"+p.ChargeID]; ok {
				expected[p.Key] = *p
			}
		}
	}
	report.ExpectedPostings = len(expected)
	for _, a := range actual {
		e, ok := expected[a.Key]
		if !ok {
			kind := "unexpected_posting"
			if observed[a.Key] {
				kind = "state_mismatch"
			}
			copyA := a
			report.Issues = append(report.Issues, Issue{Kind: kind, Key: a.Key, Actual: &copyA})
			continue
		}
		if a != e {
			copyA, copyE := a, e
			report.Issues = append(report.Issues, Issue{Kind: "value_mismatch", Key: a.Key, Expected: &copyE, Actual: &copyA})
		}
		delete(expected, a.Key)
	}
	for key, p := range expected {
		copyP := p
		report.Issues = append(report.Issues, Issue{Kind: "missing_posting", Key: key, Expected: &copyP})
	}
	sort.Slice(report.Issues, func(i, j int) bool { return report.Issues[i].Key < report.Issues[j].Key })
	return report, nil
}
func (s *Store) Reconcile(ctx context.Context, provider Provider, cutoff time.Time) (Report, error) {
	snapshot, err := provider.Snapshot(ctx, cutoff)
	if err != nil {
		return Report{}, err
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT posting_key,kind,object_id,charge_id,amount,currency,provider_created FROM journal WHERE provider_created<=$1 ORDER BY posting_key`, cutoff.Unix())
	if err != nil {
		return Report{}, err
	}
	var actual []Posting
	for rows.Next() {
		var p Posting
		if err = rows.Scan(&p.Key, &p.Kind, &p.ObjectID, &p.ChargeID, &p.Amount, &p.Currency, &p.Created); err != nil {
			rows.Close()
			return Report{}, err
		}
		actual = append(actual, p)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Report{}, err
	}
	report, err := Compare(snapshot, actual, cutoff)
	if err != nil {
		return report, err
	}
	report.Source = "custom_provider"
	if named, ok := provider.(interface{ Source() string }); ok {
		report.Source = named.Source()
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return report, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO reconciliation_runs(cutoff,report) VALUES($1,$2)`, cutoff, raw); err != nil {
		return report, err
	}
	return report, tx.Commit(ctx)
}
