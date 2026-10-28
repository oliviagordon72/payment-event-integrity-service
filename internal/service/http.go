package service

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type API struct {
	Store                     *Store
	WebhookSecret, AdminToken string
}

func (a API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Store.DB.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /webhooks/stripe", a.webhook)
	mux.HandleFunc("GET /v1/balances", a.authorize(a.balances))
	mux.HandleFunc("GET /v1/events/{id}", a.authorize(a.event))
	return mux
}
func (a API) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.AdminToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+a.AdminToken)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		next(w, r)
	}
}
func (a API) webhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid or oversized body", 413)
		return
	}
	if err = VerifySignature(raw, r.Header.Get("Stripe-Signature"), a.WebhookSecret, time.Now()); err != nil {
		http.Error(w, "invalid signature", 400)
		return
	}
	e, err := Decode(raw, a.Store.Version)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	duplicate, err := a.Store.Enqueue(r.Context(), e, raw)
	if err != nil {
		http.Error(w, "event not persisted; retry delivery", 503)
		return
	}
	writeJSON(w, 202, map[string]any{"event_id": e.ID, "duplicate": duplicate})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func (a API) balances(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Store.DB.Query(r.Context(), `SELECT account,currency,SUM(signed_amount)::bigint FROM ledger_entries GROUP BY account,currency ORDER BY currency,account`)
	if err != nil {
		http.Error(w, "query failed", 503)
		return
	}
	defer rows.Close()
	type balance struct {
		Account          string `json:"account"`
		Currency         string `json:"currency"`
		DebitMinusCredit int64  `json:"debit_minus_credit"`
	}
	result := []balance{}
	for rows.Next() {
		var b balance
		if err = rows.Scan(&b.Account, &b.Currency, &b.DebitMinusCredit); err != nil {
			http.Error(w, "query failed", 503)
			return
		}
		result = append(result, b)
	}
	if rows.Err() != nil {
		http.Error(w, "query failed", 503)
		return
	}
	writeJSON(w, 200, result)
}
func (a API) event(w http.ResponseWriter, r *http.Request) {
	var state string
	var attempts int
	var lastError *string
	err := a.Store.DB.QueryRow(r.Context(), `SELECT state,attempts,last_error FROM inbox WHERE event_id=$1`, r.PathValue("id")).Scan(&state, &attempts, &lastError)
	if err != nil {
		http.Error(w, "event unavailable", 404)
		return
	}
	writeJSON(w, 200, map[string]any{"state": state, "attempts": attempts, "last_error": lastError})
}
