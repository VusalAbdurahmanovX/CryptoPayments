package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

func webhookSignature(secret, timestamp string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(timestamp))
	h.Write([]byte("."))
	h.Write(body)
	return "sha256=" + hex.EncodeToString(h.Sum(nil))
}

type Dispatcher struct {
	Store  *Store
	Client *http.Client
}

func newDispatcher(s *Store) *Dispatcher {
	return &Dispatcher{Store: s, Client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (d *Dispatcher) DeliverOne(ctx context.Context) (bool, error) {
	if d.Store.Config.WebhookURL == "" {
		return false, nil
	}
	var id string
	var body []byte
	var attempts int
	// Preserve event order per invoice; a failed earlier event blocks newer ones.
	err := d.Store.DB.QueryRow(`SELECT e.id,e.payload,e.attempts FROM events e
WHERE e.delivered_at IS NULL AND e.next_attempt<=? AND NOT EXISTS(
 SELECT 1 FROM events older WHERE older.invoice_id=e.invoice_id AND older.delivered_at IS NULL AND older.rowid<e.rowid)
ORDER BY e.rowid LIMIT 1`, unixNow()).Scan(&id, &body, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stamp := strconv.FormatInt(unixNow(), 10)
	req, err := http.NewRequestWithContext(ctx, "POST", d.Store.Config.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return false, errors.New("invalid webhook configuration")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CryptoPayments-Event-ID", id)
	req.Header.Set("X-CryptoPayments-Timestamp", stamp)
	req.Header.Set("X-CryptoPayments-Signature", webhookSignature(d.Store.Config.WebhookSecret, stamp, body))
	resp, sendErr := d.Client.Do(req)
	ok := false
	if sendErr == nil {
		ok = resp.StatusCode >= 200 && resp.StatusCode < 300
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
	if ok {
		_, err = d.Store.DB.Exec("UPDATE events SET delivered_at=?,attempts=attempts+1 WHERE id=?", unixNow(), id)
	} else {
		power := attempts
		if power > 10 {
			power = 10
		}
		delay := int64(1<<power) * 5
		if delay > 3600 {
			delay = 3600
		}
		_, err = d.Store.DB.Exec("UPDATE events SET attempts=attempts+1,next_attempt=? WHERE id=?", unixNow()+delay, id)
	}
	return true, err
}

func (d *Dispatcher) Run(ctx context.Context) {
	for {
		worked, err := d.DeliverOne(ctx)
		if ctx.Err() != nil {
			return
		}
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
