package main

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/pbkdf2"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	xpub, err := publicBranch(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return Config{Database: filepath.Join(t.TempDir(), "payments.sqlite"), ChainID: 97, Token: "0x" + strings.Repeat("1", 40), Xpub: xpub.String(), APIKey: strings.Repeat("k", 32), Confirmations: 3, BatchBlocks: 500, InvoiceTTL: 3600}
}
func testStore(t *testing.T, c Config) *Store {
	t.Helper()
	s, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close() })
	return s
}
func mustAmount(t *testing.T, value string) *big.Int {
	t.Helper()
	n, err := parseAmount(value)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func mustInvoice(t *testing.T, s *Store, order, amount string, start uint64, now int64) Invoice {
	t.Helper()
	i, _, err := s.Create(order, mustAmount(t, amount), start, now)
	if err != nil {
		t.Fatal(err)
	}
	return i
}
func mustGet(t *testing.T, s *Store, id string) Invoice {
	t.Helper()
	i, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestAmounts(t *testing.T) {
	for _, value := range []string{"1", "0.000000000000000001", "12345678901234567890.123456789012345678", "1.2300"} {
		n, err := parseAmount(value)
		if err != nil {
			t.Fatal(value, err)
		}
		again, err := parseAmount(formatAmount(n))
		if err != nil || n.Cmp(again) != 0 {
			t.Fatal("amount roundtrip", value)
		}
	}
	for _, value := range []string{"0", "-1", "1e18", "1.0000000000000000001", "01", ".1", "1.", " 1", "NaN", strings.Repeat("9", 60)} {
		if _, err := parseAmount(value); err == nil {
			t.Fatalf("accepted invalid amount %q", value)
		}
	}
	if formatAmount(new(big.Int)) != "0" {
		t.Fatal("zero formatting")
	}
}

func TestWalletEthereumVector(t *testing.T) {
	// Widely used Hardhat development mnemonic, NEVER used for real wallets.
	seed := pbkdf2.Key([]byte("test test test test test test test test test test test junk"), []byte("mnemonic"), 2048, 64, sha512.New)
	xpub, err := publicBranch(seed)
	if err != nil {
		t.Fatal(err)
	}
	a, err := depositAddress(xpub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a != "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266" {
		t.Fatalf("wrong Ethereum derivation: %s", a)
	}
	a, err = depositAddress(xpub, 1)
	if err != nil || a != "0x70997970c51812dc3a010c7d01b50e0d17dc79c8" {
		t.Fatalf("second address: %s %v", a, err)
	}
	if _, err = depositAddress(xpub, 1<<31); err == nil {
		t.Fatal("accepted hardened child")
	}
}

func TestConcurrentUniqueAddressesAndIdempotency(t *testing.T) {
	s := testStore(t, testConfig(t))
	var wg sync.WaitGroup
	for n := 0; n < 24; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _, err := s.Create(fmt.Sprintf("order-%d", n), big.NewInt(1), 100, unixNow())
			if err != nil {
				t.Error(err)
			}
		}(n)
	}
	wg.Wait()
	var count, distinct int
	if err := s.DB.QueryRow("SELECT count(*),count(DISTINCT address) FROM invoices").Scan(&count, &distinct); err != nil {
		t.Fatal(err)
	}
	if count != 24 || distinct != 24 {
		t.Fatal(count, distinct)
	}
	a, created, err := s.Create("order-0", big.NewInt(1), 999, unixNow())
	if err != nil || created {
		t.Fatal(created, err)
	}
	b, _, _ := s.Create("order-0", big.NewInt(1), 999, unixNow())
	if a.ID != b.ID {
		t.Fatal("idempotency failed")
	}
	if _, _, err = s.Create("order-0", big.NewInt(2), 999, unixNow()); err != errConflict {
		t.Fatal("missing amount conflict", err)
	}
}

func TestDatabaseIdentityAndRestart(t *testing.T) {
	c := testConfig(t)
	s, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	a := mustInvoice(t, s, "first", "5", 10, unixNow())
	s.DB.Close()
	s, err = openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	b := mustInvoice(t, s, "second", "5", 10, unixNow())
	if b.Index != a.Index+1 || a.Address == b.Address {
		t.Fatal("index lost after restart")
	}
	s.DB.Close()
	c.ChainID = 56
	if s, err = openStore(c); err == nil {
		s.DB.Close()
		t.Fatal("database accepted network switch")
	}
}

func TestLedgerSplitReplayOverpaymentLateAndExpiry(t *testing.T) {
	s := testStore(t, testConfig(t))
	now := unixNow()
	i := mustInvoice(t, s, "split", "10", 100, now)
	p := Payment{TxHash: "0x" + strings.Repeat("a", 64), LogIndex: 1, Block: 100, BlockHash: "0x" + strings.Repeat("b", 64), Time: now, Amount: "4"}
	if err := s.apply(i, []Payment{p}, 101, now); err != nil {
		t.Fatal(err)
	}
	i = mustGet(t, s, i.ID)
	if i.Status != "partially_paid" || i.Received != "4" || i.IsPaid {
		t.Fatal(i)
	}
	if err := s.apply(i, []Payment{p}, 102, now); err != nil {
		t.Fatal(err)
	}
	i = mustGet(t, s, i.ID)
	if i.Received != "4" || len(i.Transfers) != 1 {
		t.Fatal("duplicate credit", i)
	}
	p.LogIndex = 2
	p.Block = 102
	p.Amount = "6"
	if err := s.apply(i, []Payment{p}, 103, now); err != nil {
		t.Fatal(err)
	}
	i = mustGet(t, s, i.ID)
	if i.Status != "paid" || i.Received != "10" || !i.IsPaid {
		t.Fatal(i)
	}
	p.LogIndex = 3
	p.Block = 103
	p.Amount = "2"
	p.Time = now + 7200
	if err := s.apply(i, []Payment{p}, 104, now+7200); err != nil {
		t.Fatal(err)
	}
	i = mustGet(t, s, i.ID)
	if i.Status != "overpaid" || i.Received != "12" || !i.IsPaid {
		t.Fatal("later topup must not make an on-time payment late", i)
	}
	late := mustInvoice(t, s, "late", "1", 100, now-7200)
	if err := s.expire(now); err != nil {
		t.Fatal(err)
	}
	late = mustGet(t, s, late.ID)
	if late.Status != "expired" {
		t.Fatal(late)
	}
	p.TxHash = "0x" + strings.Repeat("c", 64)
	p.LogIndex = 0
	p.Amount = "1"
	p.Time = now
	if err := s.apply(late, []Payment{p}, 101, now); err != nil {
		t.Fatal(err)
	}
	if v := mustGet(t, s, late.ID); v.Status != "paid_late" || !v.IsPaid {
		t.Fatal("late payment missing")
	}
	// A payment included before expiry remains on time even if confirmed afterwards.
	onTime := mustInvoice(t, s, "on-time-confirmed-late", "1", 100, now-7200)
	p.TxHash = "0x" + strings.Repeat("d", 64)
	p.Time = onTime.ExpiresAt - 1
	if err := s.apply(onTime, []Payment{p}, 101, now); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, s, onTime.ID).Status != "paid" {
		t.Fatal("confirmation time incorrectly used as payment time")
	}
}

type fakeChain struct {
	server      *httptest.Server
	head, final uint64
	logs        []TransferLog
	stamp       int64
	failLogs    bool
	chain       string
	decimals    string
}

func newFakeChain(t *testing.T) *fakeChain {
	f := &fakeChain{head: 104, final: 104, stamp: unixNow(), chain: "0x61", decimals: "0x12"}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     uint64            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var result any
		switch req.Method {
		case "eth_chainId":
			result = f.chain
		case "eth_call":
			result = f.decimals
		case "eth_getBlockByNumber":
			var tag string
			_ = json.Unmarshal(req.Params[0], &tag)
			number := f.head
			if tag == "finalized" {
				number = f.final
			} else if tag != "latest" {
				number, _ = hexUint(tag)
			}
			result = Block{Number: hexNumber(number), Hash: "0x" + fmt.Sprintf("%064x", number), Timestamp: hexNumber(uint64(f.stamp))}
		case "eth_getLogs":
			if f.failLogs {
				writeJSON(w, 200, map[string]any{"id": req.ID, "error": map[string]any{"code": -32005}})
				return
			}
			result = f.logs
			if f.logs == nil {
				result = []TransferLog{}
			}
		default:
			t.Errorf("unexpected RPC %s", req.Method)
		}
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(f.server.Close)
	return f
}
func fakeLog(c Config, i Invoice, block uint64, amount string) TransferLog {
	n, _ := parseAmount(amount)
	return TransferLog{Address: c.Token, Topics: []string{transferTopic, "0x" + strings.Repeat("0", 64), "0x" + strings.Repeat("0", 24) + i.Address[2:]}, Data: "0x" + fmt.Sprintf("%064x", n), BlockNumber: hexNumber(block), BlockHash: "0x" + fmt.Sprintf("%064x", block), TxHash: "0x" + strings.Repeat("a", 64), LogIndex: "0x0"}
}

func TestScannerFinalityAndConfirmations(t *testing.T) {
	c := testConfig(t)
	c.Confirmations = 5
	s := testStore(t, c)
	i := mustInvoice(t, s, "confirm", "10", 100, unixNow())
	f := newFakeChain(t)
	f.logs = []TransferLog{fakeLog(c, i, 100, "10")}
	scanner := &Scanner{Store: s, RPC: newRPC(f.server.URL)}
	f.head = 102
	f.final = 102
	if err := scanner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, s, i.ID).Status != "pending" {
		t.Fatal("credited before required confirmations")
	}
	f.head = 104
	f.final = 99
	if err := scanner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, s, i.ID).Status != "pending" {
		t.Fatal("credited before finality")
	}
	f.final = 100
	if err := scanner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	i = mustGet(t, s, i.ID)
	if i.Status != "paid" || i.NextBlock != 101 {
		t.Fatal(i)
	}
	if err := scanner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, s, i.ID).Received != "10" {
		t.Fatal("double credit")
	}
}

func TestScannerRejectsBadRPCWithoutAdvancing(t *testing.T) {
	for _, scenario := range []string{"wrong-token", "wrong-recipient", "noncanonical", "removed", "out-of-range", "malformed-value", "rpc-error"} {
		t.Run(scenario, func(t *testing.T) {
			c := testConfig(t)
			s := testStore(t, c)
			i := mustInvoice(t, s, "bad", "10", 100, unixNow())
			f := newFakeChain(t)
			entry := fakeLog(c, i, 100, "10")
			switch scenario {
			case "wrong-token":
				entry.Address = "0x" + strings.Repeat("2", 40)
			case "wrong-recipient":
				entry.Topics[2] = "0x" + strings.Repeat("0", 64)
			case "noncanonical":
				entry.BlockHash = "0x" + strings.Repeat("f", 64)
			case "removed":
				entry.Removed = true
			case "out-of-range":
				entry.BlockNumber = "0xffff"
			case "malformed-value":
				entry.Data = "0x1"
			case "rpc-error":
				f.failLogs = true
			}
			f.logs = []TransferLog{entry}
			scanner := &Scanner{Store: s, RPC: newRPC(f.server.URL)}
			if err := scanner.Tick(context.Background()); err == nil {
				t.Fatal("bad RPC accepted")
			}
			i = mustGet(t, s, i.ID)
			if i.NextBlock != 100 || i.Received != "0" {
				t.Fatal("failed scan advanced cursor or credited", i)
			}
		})
	}
}

func TestRPCStartupIdentity(t *testing.T) {
	c := testConfig(t)
	f := newFakeChain(t)
	rpc := newRPC(f.server.URL)
	if err := rpc.validate(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	f.chain = "0x38"
	if err := rpc.validate(context.Background(), c); err == nil {
		t.Fatal("wrong chain accepted")
	}
	f.chain = "0x61"
	f.decimals = "0x6"
	if err := rpc.validate(context.Background(), c); err == nil {
		t.Fatal("wrong decimals accepted")
	}
}

func TestAPI(t *testing.T) {
	c := testConfig(t)
	s := testStore(t, c)
	f := newFakeChain(t)
	rpc := newRPC(f.server.URL)
	scanner := &Scanner{Store: s, RPC: rpc}
	api := &API{Store: s, RPC: rpc, Scanner: scanner}
	handler := api.Handler()
	request := func(method, path, body, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("POST", "/v1/invoices", `{"order_id":"a","amount":"10"}`, ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, body := range []string{`{"order_id":"a","amount":10}`, `{"order_id":"a","amount":"1e2"}`, `{"order_id":"","amount":"1"}`, `{"order_id":"a","amount":"1","network":"tron"}`, `{"order_id":"a","amount":"1"} {}`} {
		if w := request("POST", "/v1/invoices", body, "Bearer "+c.APIKey); w.Code != 400 {
			t.Fatalf("accepted invalid input %s: %d", body, w.Code)
		}
	}
	body := `{"order_id":"a","amount":"10"}`
	w := request("POST", "/v1/invoices", body, "Bearer "+c.APIKey)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var i Invoice
	if err := json.Unmarshal(w.Body.Bytes(), &i); err != nil {
		t.Fatal(err)
	}
	if i.Network != "bsc-testnet" || i.Asset != "TEST-USDT" || i.NextBlock != 105 {
		t.Fatal(i)
	}
	if w = request("POST", "/v1/invoices", body, "Bearer "+c.APIKey); w.Code != 200 {
		t.Fatal("retry failed", w.Code)
	}
	if w = request("POST", "/v1/invoices", `{"order_id":"a","amount":"11"}`, "Bearer "+c.APIKey); w.Code != 409 {
		t.Fatal("conflicting retry accepted", w.Code)
	}
	if w = request("GET", "/v1/invoices/"+i.ID, "", "Bearer "+c.APIKey); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request("GET", "/readyz", "", ""); w.Code != 503 {
		t.Fatal("uninitialized scanner reported ready")
	}
	if err := scanner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w = request("GET", "/readyz", "", ""); w.Code != 200 {
		t.Fatal("healthy scanner not ready")
	}
}

func TestDurableSignedWebhookRetries(t *testing.T) {
	c := testConfig(t)
	c.WebhookSecret = strings.Repeat("s", 32)
	attempt := 0
	eventID := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		body, _ := io.ReadAll(r.Body)
		stamp := r.Header.Get("X-CryptoPayments-Timestamp")
		if got := r.Header.Get("X-CryptoPayments-Signature"); got != webhookSignature(c.WebhookSecret, stamp, body) {
			t.Error("bad signature")
		}
		id := r.Header.Get("X-CryptoPayments-Event-ID")
		if eventID == "" {
			eventID = id
		} else if eventID != id {
			t.Error("retry event id changed")
		}
		if attempt == 1 {
			w.WriteHeader(500)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	c.WebhookURL = server.URL
	s := testStore(t, c)
	mustInvoice(t, s, "webhook", "1", 100, unixNow())
	d := newDispatcher(s)
	if worked, err := d.DeliverOne(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if worked, _ := d.DeliverOne(context.Background()); worked {
		t.Fatal("retry ignored backoff")
	}
	_, _ = s.DB.Exec("UPDATE events SET next_attempt=0")
	if worked, err := d.DeliverOne(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if worked, _ := d.DeliverOne(context.Background()); worked {
		t.Fatal("delivered event resent")
	}
	var attempts int
	var delivered int64
	if err := s.DB.QueryRow("SELECT attempts,delivered_at FROM events").Scan(&attempts, &delivered); err != nil || attempts != 2 || delivered == 0 {
		t.Fatal(attempts, delivered, err)
	}
}
