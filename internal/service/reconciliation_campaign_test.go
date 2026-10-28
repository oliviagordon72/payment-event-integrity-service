package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// This is a controlled synthetic reconciliation experiment, not a Stripe API run.
// The expected key/type pairs below are authored by the injector, independently
// of Normalize, Compare, and Reconcile. All ledger writes use the normal worker.
type campaignExpectation struct {
	Key         string `json:"posting_key"`
	Kind        string `json:"expected_issue_kind"`
	FailureType string `json:"failure_type"`
}

type campaignDelivery struct {
	ID     string `json:"event_id"`
	Type   string `json:"event_type"`
	Object any    `json:"object"`
}

type campaignDataset struct {
	Seed             int64                 `json:"seed"`
	BaselineProvider Snapshot              `json:"baseline_provider"`
	FaultedProvider  Snapshot              `json:"faulted_provider"`
	BaselineEvents   []campaignDelivery    `json:"baseline_events"`
	FaultedEvents    []campaignDelivery    `json:"faulted_events"`
	Expected         []campaignExpectation `json:"expected_issues"`
	AllPostingKeys   []string              `json:"all_posting_keys"`
	ControlKeys      []string              `json:"clean_control_keys"`
}

type campaignTypeResult struct {
	FailureType    string `json:"failure_type"`
	Seeded         int    `json:"seeded"`
	Detected       int    `json:"detected"`
	FalseNegatives int    `json:"false_negatives"`
}

type campaignScore struct {
	Seeded               int                  `json:"seeded_discrepancies"`
	Detected             int                  `json:"true_positives"`
	FalsePositives       int                  `json:"false_positives"`
	FalseNegatives       int                  `json:"false_negatives"`
	WrongClassifications int                  `json:"wrong_classifications"`
	DuplicateIssues      int                  `json:"duplicate_issues"`
	ControlFlags         int                  `json:"clean_control_postings_flagged"`
	ByFailureType        []campaignTypeResult `json:"by_failure_type"`
}

type campaignEvidence struct {
	Source               string                `json:"source"`
	Seed                 int64                 `json:"seed"`
	DatasetSHA256        string                `json:"dataset_sha256"`
	FailureTypes         int                   `json:"failure_types"`
	CleanLifecycles      int                   `json:"clean_control_lifecycles"`
	CleanControlPostings int                   `json:"clean_control_postings"`
	HealthyPostingKeys   int                   `json:"healthy_posting_keys"`
	Baseline             Report                `json:"baseline"`
	Faulted              Report                `json:"faulted"`
	Score                campaignScore         `json:"score"`
	GroundTruth          []campaignExpectation `json:"ground_truth"`
	Passed               bool                  `json:"passed"`
}

func buildCampaign(seed int64, perType, controls int) campaignDataset {
	d := campaignDataset{Seed: seed}
	rng := rand.New(rand.NewSource(seed))
	types := []string{"missing_capture", "missing_refund", "wrong_amount", "wrong_currency", "wrong_refund_status", "wrong_parent_charge"}
	cases := make([]string, 0, len(types)*perType+controls)
	for _, kind := range types {
		for n := 0; n < perType; n++ {
			cases = append(cases, kind)
		}
	}
	for n := 0; n < controls; n++ {
		cases = append(cases, "clean")
	}
	rng.Shuffle(len(cases), func(i, j int) { cases[i], cases[j] = cases[j], cases[i] })
	for n, kind := range cases {
		label := fmt.Sprintf("%s_%03d", kind, n)
		c := charge("ch_campaign_"+label, int64(1000+rng.Intn(9000)))
		c.Currency = []string{"usd", "eur", "gbp", "jpy"}[rng.Intn(4)]
		c.Created = 1700000000 + int64(n)
		r := refund("re_campaign_"+label, c.ID, int64(1+rng.Intn(int(c.AmountCaptured/2))))
		r.Currency = c.Currency
		r.Created = c.Created + 1000
		localCharge, localRefund := c, r
		providerRefund := r
		captureKey, refundKey := "capture:"+c.ID, "refund:"+r.ID
		d.BaselineProvider.Charges = append(d.BaselineProvider.Charges, c)
		d.FaultedProvider.Charges = append(d.FaultedProvider.Charges, c)
		d.BaselineEvents = append(d.BaselineEvents, campaignDelivery{"evt_capture_" + label, "charge.captured", c})
		d.AllPostingKeys = append(d.AllPostingKeys, captureKey)
		hasRefund := kind != "missing_capture" && kind != "wrong_currency"
		if kind == "wrong_parent_charge" {
			// Both parent charges are valid and independently reconciled. Only the
			// refund's parent differs, so it must produce one value_mismatch.
			other := c
			other.ID += "_other"
			d.BaselineProvider.Charges = append(d.BaselineProvider.Charges, other)
			d.FaultedProvider.Charges = append(d.FaultedProvider.Charges, other)
			d.BaselineEvents = append(d.BaselineEvents, campaignDelivery{"evt_other_" + label, "charge.captured", other})
			d.FaultedEvents = append(d.FaultedEvents, campaignDelivery{"evt_other_" + label, "charge.captured", other})
			d.AllPostingKeys = append(d.AllPostingKeys, "capture:"+other.ID)
			localRefund.Charge = Reference(other.ID)
		}
		expectedKind, expectedKey := "", refundKey
		switch kind {
		case "missing_capture":
			expectedKind, expectedKey = "missing_posting", captureKey
		case "missing_refund":
			expectedKind = "missing_posting"
		case "wrong_amount":
			expectedKind = "value_mismatch"
			localRefund.Amount += int64(1 + rng.Intn(50))
		case "wrong_currency":
			expectedKind, expectedKey = "value_mismatch", captureKey
			localCharge.Currency = "eur"
			if c.Currency == "eur" {
				localCharge.Currency = "usd"
			}
		case "wrong_refund_status":
			expectedKind = "state_mismatch"
			providerRefund.Status = "failed"
		case "wrong_parent_charge":
			expectedKind = "value_mismatch"
		}
		if kind != "missing_capture" {
			d.FaultedEvents = append(d.FaultedEvents, campaignDelivery{"evt_capture_" + label, "charge.captured", localCharge})
		}
		if hasRefund {
			d.BaselineProvider.Refunds = append(d.BaselineProvider.Refunds, r)
			d.FaultedProvider.Refunds = append(d.FaultedProvider.Refunds, providerRefund)
			d.BaselineEvents = append(d.BaselineEvents, campaignDelivery{"evt_refund_" + label, "refund.updated", r})
			d.AllPostingKeys = append(d.AllPostingKeys, refundKey)
			if kind != "missing_refund" {
				d.FaultedEvents = append(d.FaultedEvents, campaignDelivery{"evt_refund_" + label, "refund.updated", localRefund})
			}
		}
		if kind == "clean" {
			d.ControlKeys = append(d.ControlKeys, captureKey, refundKey)
		} else {
			d.Expected = append(d.Expected, campaignExpectation{expectedKey, expectedKind, kind})
		}
	}
	// Provider inventory order differs from event delivery order.
	rng.Shuffle(len(d.FaultedProvider.Charges), func(i, j int) {
		d.FaultedProvider.Charges[i], d.FaultedProvider.Charges[j] = d.FaultedProvider.Charges[j], d.FaultedProvider.Charges[i]
	})
	rng.Shuffle(len(d.FaultedProvider.Refunds), func(i, j int) {
		d.FaultedProvider.Refunds[i], d.FaultedProvider.Refunds[j] = d.FaultedProvider.Refunds[j], d.FaultedProvider.Refunds[i]
	})
	sort.Slice(d.Expected, func(i, j int) bool { return d.Expected[i].Key < d.Expected[j].Key })
	sort.Strings(d.AllPostingKeys)
	sort.Strings(d.ControlKeys)
	return d
}

func scoreCampaign(expected []campaignExpectation, controls []string, issues []Issue) campaignScore {
	score := campaignScore{Seeded: len(expected)}
	oracle := map[string]campaignExpectation{}
	controlSet := map[string]bool{}
	byType := map[string]*campaignTypeResult{}
	for _, key := range controls {
		controlSet[key] = true
	}
	for _, e := range expected {
		oracle[e.Key] = e
		if byType[e.FailureType] == nil {
			byType[e.FailureType] = &campaignTypeResult{FailureType: e.FailureType}
		}
		byType[e.FailureType].Seeded++
	}
	matched := map[string]bool{}
	seen := map[string]bool{}
	for _, issue := range issues {
		if controlSet[issue.Key] {
			score.ControlFlags++
		}
		pair := issue.Key + "\x00" + issue.Kind
		if seen[pair] {
			score.FalsePositives++
			score.DuplicateIssues++
			continue
		}
		seen[pair] = true
		e, ok := oracle[issue.Key]
		if !ok {
			score.FalsePositives++
			continue
		}
		if issue.Kind != e.Kind {
			score.FalsePositives++
			score.WrongClassifications++
			continue
		}
		score.Detected++
		matched[issue.Key] = true
		byType[e.FailureType].Detected++
	}
	for _, e := range expected {
		if !matched[e.Key] {
			score.FalseNegatives++
			byType[e.FailureType].FalseNegatives++
		}
	}
	for _, result := range byType {
		score.ByFailureType = append(score.ByFailureType, *result)
	}
	sort.Slice(score.ByFailureType, func(i, j int) bool { return score.ByFailureType[i].FailureType < score.ByFailureType[j].FailureType })
	return score
}

func campaignDigest(d campaignDataset) string {
	raw, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func seedCampaign(t *testing.T, s *Store, events []campaignDelivery) {
	t.Helper()
	for _, e := range events {
		enqueue(t, s, e.ID, e.Type, e.Object)
		runOne(t, s)
	}
	var count int
	if err := s.DB.QueryRow(context.Background(), `SELECT COUNT(*) FROM journal`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(events) {
		t.Fatalf("seeded %d postings, expected %d", count, len(events))
	}
	var broken int
	if err := s.DB.QueryRow(context.Background(), `SELECT COUNT(*) FROM (SELECT posting_key FROM ledger_entries GROUP BY posting_key HAVING COUNT(*)<>2 OR SUM(signed_amount)<>0) b`).Scan(&broken); err != nil || broken != 0 {
		t.Fatalf("invalid double-entry postings: %d, %v", broken, err)
	}
}

// This test runs in the existing CI suite even when no report path is set.
// With RECONCILIATION_CAMPAIGN_REPORT set, it also writes user-facing evidence.
func TestReconciliationCampaign(t *testing.T) {
	const seed int64 = 42
	const perType, controls = 20, 200
	d := buildCampaign(seed, perType, controls)
	baselineStore := testStore(t)
	seedCampaign(t, baselineStore, d.BaselineEvents)
	cutoff := time.Unix(1800000000, 0).UTC()
	baseline, err := baselineStore.Reconcile(context.Background(), FixtureProvider{Data: d.BaselineProvider}, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.Issues) != 0 {
		t.Fatalf("clean baseline has %d false positives", len(baseline.Issues))
	}
	if baseline.ExpectedPostings != 620 || baseline.ActualPostings != 620 {
		t.Fatalf("unexpected baseline counts: %+v", baseline)
	}
	// A second isolated schema contains the faulted fixture. No ledger mutation
	// guards are disabled and no existing public-schema records are modified.
	faultedStore := testStore(t)
	seedCampaign(t, faultedStore, d.FaultedEvents)
	faulted, err := faultedStore.Reconcile(context.Background(), FixtureProvider{Data: d.FaultedProvider}, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	score := scoreCampaign(d.Expected, d.ControlKeys, faulted.Issues)
	passed := score.Seeded == 120 && score.Detected == 120 && score.FalsePositives == 0 && score.FalseNegatives == 0 && score.ControlFlags == 0 && len(score.ByFailureType) == 6 && faulted.ExpectedPostings == 600 && faulted.ActualPostings == 580
	evidence := campaignEvidence{
		Source: "synthetic_postgresql_campaign", Seed: seed, DatasetSHA256: campaignDigest(d), FailureTypes: 6,
		CleanLifecycles: controls, CleanControlPostings: len(d.ControlKeys), HealthyPostingKeys: len(d.AllPostingKeys) - len(d.Expected),
		Baseline: baseline, Faulted: faulted, Score: score, GroundTruth: d.Expected, Passed: passed,
	}
	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("RECONCILIATION_CAMPAIGN_REPORT"); path != "" {
		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, append(raw, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
		t.Logf("campaign evidence: %s", path)
	}
	for _, result := range score.ByFailureType {
		t.Logf("%s: detected %d/%d; false negatives %d", result.FailureType, result.Detected, result.Seeded, result.FalseNegatives)
	}
	t.Logf("CAMPAIGN_RESULT seeded=%d detected=%d false_positives=%d false_negatives=%d clean_controls=%d passed=%v", score.Seeded, score.Detected, score.FalsePositives, score.FalseNegatives, controls, passed)
	if !passed {
		t.Fatalf("reconciliation campaign failed: %+v", score)
	}
}

func TestCampaignScoringRejectsBadReports(t *testing.T) {
	expected := []campaignExpectation{{"capture:a", "missing_posting", "missing_capture"}, {"refund:b", "value_mismatch", "wrong_amount"}}
	for _, tc := range []struct {
		name                                    string
		issues                                  []Issue
		tp, fp, fn, wrong, duplicates, controls int
	}{
		{"perfect", []Issue{{Key: "capture:a", Kind: "missing_posting"}, {Key: "refund:b", Kind: "value_mismatch"}}, 2, 0, 0, 0, 0, 0},
		{"all missed", nil, 0, 0, 2, 0, 0, 0},
		{"wrong classification", []Issue{{Key: "capture:a", Kind: "value_mismatch"}}, 0, 1, 2, 1, 0, 0},
		{"healthy flagged", []Issue{{Key: "capture:clean", Kind: "missing_posting"}}, 0, 1, 2, 0, 0, 1},
		{"duplicate flag", []Issue{{Key: "capture:a", Kind: "missing_posting"}, {Key: "capture:a", Kind: "missing_posting"}}, 1, 1, 1, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scoreCampaign(expected, []string{"capture:clean"}, tc.issues)
			if got.Detected != tc.tp || got.FalsePositives != tc.fp || got.FalseNegatives != tc.fn || got.WrongClassifications != tc.wrong || got.DuplicateIssues != tc.duplicates || got.ControlFlags != tc.controls {
				t.Fatalf("bad score: %+v", got)
			}
		})
	}
}

func TestCampaignDatasetDeterministic(t *testing.T) {
	a, b, c := buildCampaign(42, 20, 200), buildCampaign(42, 20, 200), buildCampaign(43, 20, 200)
	if campaignDigest(a) != campaignDigest(b) {
		t.Fatal("same seed changed dataset")
	}
	if campaignDigest(a) == campaignDigest(c) {
		t.Fatal("different seed did not change dataset")
	}
	if len(a.Expected) != 120 || len(a.ControlKeys) != 400 || len(a.AllPostingKeys) != 620 {
		t.Fatal("wrong fixture sizes")
	}
	seen := map[string]bool{}
	for _, e := range a.Expected {
		if seen[e.Key] {
			t.Fatalf("duplicate ground-truth key %s", e.Key)
		}
		seen[e.Key] = true
	}
}
