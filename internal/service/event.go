package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const APIVersion = "2026-09-30.endive"

type Event struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Type     string `json:"type"`
	Version  string `json:"api_version"`
	Livemode *bool  `json:"livemode"`
	Account  string `json:"account"`
	Context  string `json:"context"`
	Data     struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// VerifySignature verifies the original bytes, tolerates rotating v1 signatures,
// and rejects timestamps more than five minutes in either direction.
func VerifySignature(body []byte, header, secret string, now time.Time) error {
	if secret == "" {
		return errors.New("missing webhook secret")
	}
	var timestamp string
	var signatures []string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if kv[0] == "t" {
			if timestamp != "" {
				return errors.New("duplicate timestamp")
			}
			timestamp = kv[1]
		}
		if kv[0] == "v1" {
			signatures = append(signatures, kv[1])
		}
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || ts < now.Unix()-300 || ts > now.Unix()+300 {
		return errors.New("invalid signature timestamp")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	for _, sig := range signatures {
		b, err := hex.DecodeString(sig)
		if err == nil && hmac.Equal(mac.Sum(nil), b) {
			return nil
		}
	}
	return errors.New("invalid signature")
}

func Decode(body []byte, version string) (Event, error) {
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return e, err
	}
	if e.ID == "" || len(e.ID) > 255 || e.Object != "event" || e.Type == "" || len(e.Data.Object) == 0 {
		return e, errors.New("invalid snapshot event")
	}
	if e.Livemode == nil || *e.Livemode || e.Account != "" || e.Context != "" {
		return e, errors.New("only direct-account test-mode events are supported")
	}
	if e.Version != version {
		return e, fmt.Errorf("API version must be %s", version)
	}
	return e, nil
}

type Reference string

func (r *Reference) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*r = Reference(s)
		return nil
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	*r = Reference(obj.ID)
	return nil
}

type Charge struct {
	ID                   string `json:"id"`
	Object               string `json:"object"`
	AmountCaptured       int64  `json:"amount_captured"`
	Currency             string `json:"currency"`
	Captured             bool   `json:"captured"`
	Paid                 bool   `json:"paid"`
	Status               string `json:"status"`
	Created              int64  `json:"created"`
	Livemode             *bool  `json:"livemode"`
	PaymentMethodDetails struct {
		Type string `json:"type"`
	} `json:"payment_method_details"`
}
type Refund struct {
	ID       string    `json:"id"`
	Object   string    `json:"object"`
	Charge   Reference `json:"charge"`
	Amount   int64     `json:"amount"`
	Currency string    `json:"currency"`
	Status   string    `json:"status"`
	Created  int64     `json:"created"`
	// Stripe's Refund object doesn't expose livemode; the envelope and API key scope it.
}
type Posting struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	ObjectID string `json:"object_id"`
	ChargeID string `json:"charge_id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Created  int64  `json:"created"`
}

var currencyRE = regexp.MustCompile(`^[a-z]{3}$`)

func (p Posting) Validate() error {
	if p.Amount <= 0 || p.Amount > 99999999 || p.Created <= 0 || !currencyRE.MatchString(p.Currency) || p.ObjectID == "" || p.ChargeID == "" || len(p.ObjectID) > 255 || len(p.ChargeID) > 255 {
		return errors.New("invalid posting")
	}
	if p.Kind != "capture" && p.Kind != "refund" {
		return errors.New("invalid posting kind")
	}
	if p.Key != p.Kind+":"+p.ObjectID {
		return errors.New("invalid posting key")
	}
	return nil
}
func chargePosting(c Charge) (*Posting, error) {
	if c.Object != "charge" || c.Livemode == nil || *c.Livemode {
		return nil, errors.New("invalid test-mode charge")
	}
	if !c.Captured || !c.Paid || c.Status != "succeeded" {
		return nil, nil
	}
	if c.PaymentMethodDetails.Type != "card" {
		return nil, errors.New("only card charges are supported")
	}
	p := Posting{"capture:" + c.ID, "capture", c.ID, c.ID, c.AmountCaptured, c.Currency, c.Created}
	return &p, p.Validate()
}
func refundPosting(r Refund) (*Posting, error) {
	if r.Object != "refund" || r.ID == "" || r.Charge == "" {
		return nil, errors.New("invalid charge refund")
	}
	switch r.Status {
	case "pending", "requires_action", "failed", "canceled":
		return nil, nil
	case "succeeded":
	default:
		return nil, errors.New("unknown refund status")
	}
	p := Posting{"refund:" + r.ID, "refund", r.ID, string(r.Charge), r.Amount, r.Currency, r.Created}
	return &p, p.Validate()
}
func Normalize(e Event) (*Posting, error) {
	switch e.Type {
	case "charge.succeeded", "charge.captured":
		var c Charge
		if err := json.Unmarshal(e.Data.Object, &c); err != nil {
			return nil, err
		}
		return chargePosting(c)
	case "refund.created", "refund.updated":
		var r Refund
		if err := json.Unmarshal(e.Data.Object, &r); err != nil {
			return nil, err
		}
		return refundPosting(r)
	default:
		return nil, nil
	}
}
