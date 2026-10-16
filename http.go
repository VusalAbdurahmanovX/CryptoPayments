package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

type API struct {
	Store   *Store
	Scanner *Scanner
	RPC     *RPC
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		a.Scanner.mu.RLock()
		last, scanError := a.Scanner.LastSuccess, a.Scanner.LastError
		a.Scanner.mu.RUnlock()
		limit := int64(a.Store.Config.Poll.Seconds()) * 3
		if limit < 60 {
			limit = 60
		}
		if last == 0 || unixNow()-last > limit || scanError != "" {
			writeJSON(w, 503, map[string]string{"status": "not_ready"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ready"})
	})
	private := http.NewServeMux()
	private.HandleFunc("POST /v1/invoices", a.create)
	private.HandleFunc("GET /v1/invoices/{id}", a.get)
	private.HandleFunc("GET /v1/invoices/{id}/sweeps", func(w http.ResponseWriter, r *http.Request) {
		if _, err := a.Store.Get(r.PathValue("id")); errors.Is(err, errNotFound) {
			writeError(w, 404, "invoice not found")
			return
		} else if err != nil {
			a.internal(w, err)
			return
		}
		jobs, err := a.Store.sweepJobs(r.PathValue("id"))
		if err != nil {
			a.internal(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"sweeps": jobs})
	})
	private.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		a.Scanner.mu.RLock()
		defer a.Scanner.mu.RUnlock()
		worker, err := a.Store.workerStatus()
		if err != nil {
			a.internal(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"chain_id": a.Store.Config.ChainID, "testnet": a.Store.Config.ChainID == 97, "token_contract": a.Store.Config.Token, "confirmations": a.Store.Config.Confirmations, "last_scan_at": a.Scanner.LastSuccess, "last_scan_error": a.Scanner.LastError, "safe_block": a.Scanner.SafeBlock, "sweeper": worker})
	})
	mux.Handle("/v1/", a.auth(private))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (a *API) auth(next http.Handler) http.Handler {
	expected := sha256.Sum256([]byte("Bearer " + a.Store.Config.APIKey))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actual := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, 401, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var input struct {
		OrderID string `json:"order_id"`
		Amount  string `json:"amount"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		writeError(w, 400, "invalid JSON; expected order_id and amount strings")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, 400, "expected a single JSON object")
		return
	}
	if input.OrderID == "" || len(input.OrderID) > 200 || strings.TrimSpace(input.OrderID) != input.OrderID {
		writeError(w, 400, "order_id is required, maximum 200 bytes, without surrounding whitespace")
		return
	}
	amount, err := parseAmount(input.Amount)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	// Existing order retries work even when RPC is temporarily offline.
	existing, err := a.Store.scanInvoice(a.Store.DB.QueryRow("SELECT "+invoiceColumns+" FROM invoices WHERE order_id=?", input.OrderID))
	if err == nil {
		if existing.Amount != formatAmount(amount) {
			writeError(w, 409, errConflict.Error())
			return
		}
		i, e := a.Store.Get(existing.ID)
		if e != nil {
			a.internal(w, e)
			return
		}
		writeJSON(w, 200, i)
		return
	}
	if !errors.Is(err, errNotFound) {
		a.internal(w, err)
		return
	}
	block, err := a.RPC.block(r.Context(), "latest")
	if err != nil {
		writeError(w, 503, "blockchain RPC unavailable; retry with the same order_id")
		return
	}
	n, _ := hexUint(block.Number)
	i, created, err := a.Store.Create(input.OrderID, amount, n+1, unixNow())
	if errors.Is(err, errConflict) {
		writeError(w, 409, err.Error())
		return
	}
	if err != nil {
		a.internal(w, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	w.Header().Set("Location", "/v1/invoices/"+i.ID)
	writeJSON(w, status, i)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	i, err := a.Store.Get(r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		writeError(w, 404, "invoice not found")
		return
	}
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, 200, i)
}

func (a *API) internal(w http.ResponseWriter, err error) {
	log.Printf("API database error: %v", err)
	writeError(w, 500, "internal error")
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
