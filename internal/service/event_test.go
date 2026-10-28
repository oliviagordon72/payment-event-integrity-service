package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func signature(raw []byte, secret string, at time.Time) string {
	ts := fmt.Sprint(at.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(raw)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
func eventRaw(id, kind string, obj any) []byte {
	raw, _ := json.Marshal(map[string]any{"id": id, "object": "event", "type": kind, "api_version": APIVersion, "livemode": false, "data": map[string]any{"object": obj}})
	return raw
}
func charge(id string, amount int64) Charge {
	no := false
	c := Charge{ID: id, Object: "charge", AmountCaptured: amount, Currency: "usd", Captured: true, Paid: true, Status: "succeeded", Created: 1700000000, Livemode: &no}
	c.PaymentMethodDetails.Type = "card"
	return c
}
func refund(id, ch string, amount int64) Refund {
	return Refund{ID: id, Object: "refund", Charge: Reference(ch), Amount: amount, Currency: "usd", Status: "succeeded", Created: 1700000010}
}
func TestSignature(t *testing.T) {
	now := time.Unix(1800000000, 0)
	raw := []byte(`{"hello":"world"}`)
	secret := "whsec_test"
	cases := []struct {
		name   string
		body   []byte
		header string
		ok     bool
	}{
		{"valid", raw, signature(raw, secret, now), true},
		{"rotating signatures", raw, signature(raw, secret, now) + ",v1=00", true},
		{"tampered", append(append([]byte{}, raw...), 32), signature(raw, secret, now), false},
		{"stale", raw, signature(raw, secret, now.Add(-301*time.Second)), false},
		{"future", raw, signature(raw, secret, now.Add(301*time.Second)), false},
		{"wrong secret", raw, signature(raw, "other", now), false},
		{"missing", raw, "", false},
		{"overflow timestamp", raw, "t=9223372036854775807,v1=00", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ok := VerifySignature(tc.body, tc.header, secret, now) == nil; ok != tc.ok {
				t.Fatalf("got valid=%v want %v", ok, tc.ok)
			}
		})
	}
}
func TestNormalize(t *testing.T) {
	c := charge("ch_a", 1000)
	r := refund("re_a", "ch_a", 200)
	for _, kind := range []string{"charge.succeeded", "charge.captured"} {
		e, err := Decode(eventRaw("evt_a", kind, c), APIVersion)
		if err != nil {
			t.Fatal(err)
		}
		p, err := Normalize(e)
		if err != nil || p == nil || p.Amount != 1000 {
			t.Fatalf("%+v %v", p, err)
		}
	}
	r.Status = "pending"
	e, _ := Decode(eventRaw("evt_r", "refund.created", r), APIVersion)
	if p, err := Normalize(e); p != nil || err != nil {
		t.Fatal(p, err)
	}
	r.Status = "succeeded"
	r.Amount = 0
	e, _ = Decode(eventRaw("evt_r", "refund.updated", r), APIVersion)
	if _, err := Normalize(e); err == nil {
		t.Fatal("zero money accepted")
	}
	for key, value := range map[string]any{"livemode": true, "api_version": "old", "account": "acct_other"} {
		var data map[string]any
		json.Unmarshal(eventRaw("evt_a", "charge.succeeded", c), &data)
		data[key] = value
		raw, _ := json.Marshal(data)
		if _, err := Decode(raw, APIVersion); err == nil {
			t.Fatalf("accepted %s", key)
		}
	}
	var ref Reference
	if err := json.Unmarshal([]byte(`{"id":"ch_a"}`), &ref); err != nil || ref != "ch_a" {
		t.Fatal(ref, err)
	}
}
func FuzzVerifySignature(f *testing.F) {
	f.Add([]byte(`{"id":"evt_test"}`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		now := time.Unix(1800000000, 0)
		h := signature(raw, "secret", now)
		if err := VerifySignature(raw, h, "secret", now); err != nil {
			t.Fatal(err)
		}
		changed := append(append([]byte{}, raw...), 0)
		if VerifySignature(changed, h, "secret", now) == nil {
			t.Fatal("tampered data accepted")
		}
	})
}
func FuzzDecodeNormalize(f *testing.F) {
	f.Add(eventRaw("evt_seed", "charge.captured", charge("ch_seed", 1000)))
	f.Add(eventRaw("evt_seed", "refund.updated", refund("re_seed", "ch_seed", 100)))
	f.Fuzz(func(t *testing.T, raw []byte) {
		e, err := Decode(raw, APIVersion)
		if err != nil {
			return
		}
		p, err := Normalize(e)
		if err == nil && p != nil {
			if err = p.Validate(); err != nil {
				t.Fatalf("normalizer returned invalid money: %v", err)
			}
		}
	})
}
