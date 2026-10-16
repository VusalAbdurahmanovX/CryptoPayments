package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func signerFixture(t *testing.T) (*Sweeper, Config) {
	t.Helper()
	c := testConfig(t)
	store := testStore(t, c)
	seed := bytes.Repeat([]byte{7}, 32)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	seedFile := filepath.Join(t.TempDir(), "seed")
	gasFile := filepath.Join(t.TempDir(), "gas")
	if err = os.WriteFile(seedFile, []byte(hex.EncodeToString(seed)), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(gasFile, []byte(hex.EncodeToString(crypto.FromECDSA(key))), 0600); err != nil {
		t.Fatal(err)
	}
	sc := SweepConfig{Treasury: "0x" + strings.Repeat("2", 40), SeedFile: seedFile, GasKeyFile: gasFile, TokenGas: 100000, MaxGasPrice: big.NewInt(10000000000), MaxTopup: big.NewInt(2000000000000000), MinAmount: new(big.Int)}
	s, err := newSweeper(store, nil, sc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, c
}

func confirmedDeposit(t *testing.T, s *Store, order, amount string) Invoice {
	t.Helper()
	i := mustInvoice(t, s, order, "10", 100, unixNow())
	id := fmt.Sprintf("0x%064x", i.Index+1)
	p := Payment{TxHash: id, LogIndex: 0, Block: 100, BlockHash: "0x" + fmt.Sprintf("%064x", 100), Time: unixNow(), Amount: amount}
	if err := s.apply(i, []Payment{p}, 101, unixNow()); err != nil {
		t.Fatal(err)
	}
	return mustGet(t, s, i.ID)
}

func TestSignerPrivateKeyMatchesDepositAndRejectsWrongSeed(t *testing.T) {
	s, _ := signerFixture(t)
	for _, index := range []uint32{0, 17, 1001} {
		key, err := privateAt(s.Seed, index)
		if err != nil {
			t.Fatal(err)
		}
		address, err := depositAddress(s.Store.Xpub, index)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(crypto.PubkeyToAddress(key.PublicKey).Hex(), address) {
			t.Fatal("wrong spending key")
		}
		key.D.SetInt64(0)
	}
	if err := os.WriteFile(s.Config.SeedFile, []byte(strings.Repeat("11", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	if worker, err := newSweeper(s.Store, nil, s.Config); err == nil {
		worker.Close()
		t.Fatal("accepted unrelated deposit seed")
	}
	if err := os.Chmod(s.Config.GasKeyFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecret(s.Config.GasKeyFile); err == nil {
		t.Fatal("accepted world-readable private key")
	}
}

func TestSweepQueueUsesConfirmedPartialFundsAndCompletedLedger(t *testing.T) {
	s, _ := signerFixture(t)
	i := confirmedDeposit(t, s.Store, "partial-sweep", "4")
	if i.IsPaid {
		t.Fatal("invoice should remain partially paid")
	}
	j, err := s.Store.nextSweep(s.Config.Treasury, new(big.Int))
	if err != nil {
		t.Fatal(err)
	}
	if j.Amount != "4" {
		t.Fatal(j)
	}
	if _, err = s.Store.DB.Exec("UPDATE sweep_jobs SET status='completed' WHERE id=?", j.ID); err != nil {
		t.Fatal(err)
	}
	p := Payment{TxHash: "0x" + strings.Repeat("f", 64), Block: 102, BlockHash: "0x" + strings.Repeat("b", 64), Time: unixNow(), Amount: "6"}
	if err = s.Store.apply(i, []Payment{p}, 103, unixNow()); err != nil {
		t.Fatal(err)
	}
	j, err = s.Store.nextSweep(s.Config.Treasury, new(big.Int))
	if err != nil {
		t.Fatal(err)
	}
	if j.Amount != "6" || j.InvoiceID != i.ID {
		t.Fatal("swept already collected funds again", j)
	}
}

func TestSignerLockExcludesSecondProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.lock")
	lock, err := sweepLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := sweepLock(path); err == nil {
		second.Close()
		t.Fatal("second signer acquired lock")
	}
	lock.Close()
	lock, err = sweepLock(path)
	if err != nil {
		t.Fatal("lock not released", err)
	}
	lock.Close()
}

func TestSignedTransactionPersistedBeforeBroadcastAndSameHashRetried(t *testing.T) {
	s, c := signerFixture(t)
	i := confirmedDeposit(t, s.Store, "retry", "10")
	j, err := s.Store.nextSweep(s.Config.Treasury, new(big.Int))
	if err != nil {
		t.Fatal(err)
	}
	sends := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     uint64            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch request.Method {
		case "eth_getTransactionCount":
			result = "0x0"
		case "eth_getTransactionReceipt":
			result = nil
		case "eth_sendRawTransaction":
			var raw string
			_ = json.Unmarshal(request.Params[0], &raw)
			var saved string
			if err := s.Store.DB.QueryRow("SELECT raw_tx FROM sweep_transactions WHERE job_id=?", j.ID).Scan(&saved); err != nil || saved != raw {
				t.Error("broadcast preceded durable transaction record", err)
			}
			data, e := hex.DecodeString(strings.TrimPrefix(raw, "0x"))
			if e != nil {
				t.Error(e)
			}
			var tx types.Transaction
			if e = tx.UnmarshalBinary(data); e != nil {
				t.Error(e)
			}
			if tx.ChainId().Uint64() != c.ChainID || tx.To() == nil || *tx.To() != common.HexToAddress(i.Address) {
				t.Error("wrong signed destination/chain")
			}
			from, e := types.Sender(types.NewEIP155Signer(new(big.Int).SetUint64(c.ChainID)), &tx)
			if e != nil || !strings.EqualFold(from.Hex(), s.GasAddress) {
				t.Error("wrong signer", e)
			}
			sends = append(sends, raw)
			result = tx.Hash().Hex()
			if len(sends) == 1 {
				writeJSON(w, 200, map[string]any{"id": request.ID, "error": map[string]int{"code": -32000}})
				return
			}
		default:
			t.Error("unexpected RPC", request.Method)
		}
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer server.Close()
	s.RPC = newRPC(server.URL)
	if err = s.prepare(context.Background(), j, "gas", s.GasKey, i.Address, big.NewInt(1000000), nil, 21000, big.NewInt(1)); err != nil {
		t.Fatal(err)
	}
	if len(sends) != 0 {
		t.Fatal("prepare broadcast prematurely")
	}
	if err = s.process(context.Background(), j, 100); err == nil {
		t.Fatal("expected ambiguous acknowledgement")
	}
	// Reopen the database to model an actual signer restart after the ambiguous send.
	s.Store.DB.Close()
	s.Store = testStore(t, c)
	if err = s.process(context.Background(), j, 100); err != nil {
		t.Fatal(err)
	}
	if len(sends) != 2 || sends[0] != sends[1] {
		t.Fatal("restart signed/sent a new transaction")
	}
	jobs, err := s.Store.sweepJobs(i.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || len(jobs[0].Transactions) != 1 || jobs[0].Transactions[0].State != "prepared" {
		t.Fatal(jobs)
	}
	encoded, _ := json.Marshal(jobs)
	if bytes.Contains(encoded, []byte(sends[0])) {
		t.Fatal("API leaked raw transaction")
	}
}

func TestReceiptWithoutExpectedTokenTransferRequiresReview(t *testing.T) {
	s, _ := signerFixture(t)
	confirmedDeposit(t, s.Store, "false-token", "10")
	j, err := s.Store.nextSweep(s.Config.Treasury, new(big.Int))
	if err != nil {
		t.Fatal(err)
	}
	tx := SweepTransaction{JobID: j.ID, Hash: "0x" + strings.Repeat("a", 64), Kind: "token", Sender: "0x" + strings.Repeat("3", 40), Amount: "10000000000000000000"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result any
		if req.Method == "eth_getTransactionReceipt" {
			result = sweepReceipt{Hash: tx.Hash, Block: "0x64", BlockHash: "0x" + fmt.Sprintf("%064x", 100), Status: "0x1", Logs: []TransferLog{}}
		} else if req.Method == "eth_getBlockByNumber" {
			result = Block{Number: "0x64", Timestamp: "0x64", Hash: "0x" + fmt.Sprintf("%064x", 100)}
		} else {
			t.Error("unexpected RPC", req.Method)
		}
		writeJSON(w, 200, map[string]any{"id": req.ID, "result": result})
	}))
	defer server.Close()
	s.RPC = newRPC(server.URL)
	if err = s.settleOrBroadcast(context.Background(), j, tx, 100); err == nil {
		t.Fatal("accepted success without token transfer")
	}
	jobs, err := s.Store.sweepJobs(j.InvoiceID)
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].Status != "review" {
		t.Fatal(jobs)
	}
}
