package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOverpaymentWebhookIsPaid(t *testing.T) {
	c := testConfig(t)
	c.WebhookURL = "http://unused.local/webhook"
	c.WebhookSecret = strings.Repeat("s", 32)
	s := testStore(t, c)
	i := mustInvoice(t, s, "overpaid-webhook", "10", 100, unixNow())
	p := Payment{TxHash: "0x" + strings.Repeat("a", 64), LogIndex: 0, Block: 100, BlockHash: "0x" + strings.Repeat("b", 64), Time: unixNow(), Amount: "12"}
	if err := s.apply(i, []Payment{p}, 101, unixNow()); err != nil {
		t.Fatal(err)
	}
	var body []byte
	if err := s.DB.QueryRow("SELECT payload FROM events WHERE invoice_id=? ORDER BY rowid DESC LIMIT 1", i.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Type    string  `json:"type"`
		Invoice Invoice `json:"invoice"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "invoice.updated" || !event.Invoice.IsPaid || event.Invoice.Status != "overpaid" || event.Invoice.Received != "12" {
		t.Fatalf("incorrect payment webhook: %s", body)
	}
}
