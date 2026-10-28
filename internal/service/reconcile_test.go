package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStripeRejectsIncompleteInventory(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"API failure", `{"error":{}}`, 429},
		{"empty continuation", `{"object":"list","data":[],"has_more":true}`, 200},
		{"malformed JSON", `{`, 200},
		{"wrong response type", `{"object":"charge"}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			if _, err := (Stripe{Key: "sk_test_fake", Client: client}).Snapshot(context.Background(), time.Now()); err == nil {
				t.Fatal("incomplete inventory accepted")
			}
		})
	}
}
func TestStripePagination(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		user, _, ok := req.BasicAuth()
		if !ok || user != "sk_test_fake" || req.Header.Get("Stripe-Version") != APIVersion {
			t.Fatal("missing authentication or API version")
		}
		if req.URL.Query().Get("created[lte]") != "1800000000" {
			t.Fatal("cutoff missing")
		}
		more := false
		var data []any
		if req.URL.Path == "/v1/charges" {
			if req.URL.Query().Get("starting_after") == "" {
				more = true
				data = []any{charge("ch_a", 1000)}
			} else {
				if req.URL.Query().Get("starting_after") != "ch_a" {
					t.Fatal("bad cursor")
				}
				data = []any{charge("ch_b", 2000)}
			}
		} else {
			data = []any{refund("re_a", "ch_a", 100)}
		}
		b, _ := json.Marshal(map[string]any{"object": "list", "data": data, "has_more": more})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b))), Header: http.Header{}}, nil
	})}
	snapshot, err := (Stripe{Key: "sk_test_fake", Client: client}).Snapshot(context.Background(), time.Unix(1800000000, 0))
	if err != nil || len(snapshot.Charges) != 2 || len(snapshot.Refunds) != 1 || requests != 3 {
		t.Fatalf("%+v requests=%d %v", snapshot, requests, err)
	}
	if _, err = (Stripe{Key: "sk_live_not_allowed", Client: client}).Snapshot(context.Background(), time.Now()); err == nil {
		t.Fatal("live key accepted")
	}
}
func TestReconciliationDiscrepancies(t *testing.T) {
	c := charge("ch_a", 1000)
	r := refund("re_a", "ch_a", 100)
	pc, _ := chargePosting(c)
	pr, _ := refundPosting(r)
	cases := []struct {
		name     string
		snapshot Snapshot
		actual   []Posting
		kind     string
	}{
		{"clean", Snapshot{[]Charge{c}, []Refund{r}}, []Posting{*pc, *pr}, ""},
		{"missing capture", Snapshot{Charges: []Charge{c}}, nil, "missing_posting"},
		{"missing refund", Snapshot{[]Charge{c}, []Refund{r}}, []Posting{*pc}, "missing_posting"},
		{"unknown posting", Snapshot{}, []Posting{*pc}, "unexpected_posting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, err := Compare(tc.snapshot, tc.actual, time.Unix(1800000000, 0))
			if err != nil {
				t.Fatal(err)
			}
			if tc.kind == "" {
				if len(report.Issues) != 0 {
					t.Fatal(report)
				}
			} else if len(report.Issues) != 1 || report.Issues[0].Kind != tc.kind {
				t.Fatal(report)
			}
		})
	}
	for _, field := range []string{"amount", "currency", "status", "charge"} {
		t.Run(field, func(t *testing.T) {
			changed := r
			switch field {
			case "amount":
				changed.Amount++
			case "currency":
				changed.Currency = "eur"
			case "status":
				changed.Status = "failed"
			case "charge":
				changed.Charge = "ch_other"
			}
			report, err := Compare(Snapshot{[]Charge{c}, []Refund{changed}}, []Posting{*pc, *pr}, time.Unix(1800000000, 0))
			if err != nil || len(report.Issues) != 1 {
				t.Fatal(report, err)
			}
			want := "value_mismatch"
			if field == "status" || field == "charge" {
				want = "state_mismatch"
			}
			if report.Issues[0].Kind != want {
				t.Fatal(report)
			}
		})
	}
}
