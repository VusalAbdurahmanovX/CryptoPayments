package main

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

type Sweeper struct {
	Store      *Store
	RPC        *RPC
	Config     SweepConfig
	Seed       []byte
	GasKey     *ecdsa.PrivateKey
	GasAddress string
}

func newSweeper(s *Store, rpc *RPC, c SweepConfig) (*Sweeper, error) {
	seed, err := readSecret(c.SeedFile)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			for i := range seed {
				seed[i] = 0
			}
		}
	}()
	pub, err := publicBranch(seed)
	if err != nil {
		return nil, err
	}
	if pub.String() != s.Config.Xpub {
		return nil, errors.New("sweep seed does not match receiving WALLET_XPUB")
	}
	gasBytes, err := readSecret(c.GasKeyFile)
	if err != nil {
		return nil, err
	}
	defer func() {
		for i := range gasBytes {
			gasBytes[i] = 0
		}
	}()
	gasKey, err := crypto.ToECDSA(gasBytes)
	if err != nil {
		return nil, errors.New("invalid gas private key")
	}
	gasAddress := strings.ToLower(crypto.PubkeyToAddress(gasKey.PublicKey).Hex())
	var count int
	if err = s.DB.QueryRow("SELECT count(*) FROM invoices WHERE address IN (?,?)", gasAddress, c.Treasury).Scan(&count); err != nil {
		return nil, err
	}
	if count != 0 {
		return nil, errors.New("gas and main wallet cannot be deposit addresses")
	}
	ok = true
	return &Sweeper{Store: s, RPC: rpc, Config: c, Seed: seed, GasKey: gasKey, GasAddress: gasAddress}, nil
}

func (s *Sweeper) Close() {
	for i := range s.Seed {
		s.Seed[i] = 0
	}
	s.GasKey.D.SetInt64(0)
}

// Shared-volume flock prevents two signer processes allocating the same nonce.
func sweepLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another sweep worker owns this database")
	}
	return f, nil
}

func (s *Sweeper) Run(ctx context.Context) error {
	lock, err := sweepLock(s.Store.Config.Database + ".sweep.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		_ = s.Tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.Config.Poll):
		}
	}
}

func (s *Sweeper) Tick(ctx context.Context) (err error) {
	defer func() {
		message := ""
		if err != nil {
			message = err.Error()
		}
		_, _ = s.Store.DB.Exec("INSERT INTO sweep_worker(id,treasury,gas_address,last_tick,last_error) VALUES(1,?,?,?,?) ON CONFLICT(id) DO UPDATE SET treasury=excluded.treasury,gas_address=excluded.gas_address,last_tick=excluded.last_tick,last_error=excluded.last_error", s.Config.Treasury, s.GasAddress, unixNow(), message)
	}()
	var chain string
	if err = s.RPC.call(ctx, "eth_chainId", []any{}, &chain); err != nil {
		return err
	}
	id, e := hexUint(chain)
	if e != nil || id != s.Store.Config.ChainID {
		return errors.New("sweep RPC chain mismatch")
	}
	safe, err := s.safeBlock(ctx)
	if err != nil {
		return err
	}
	j, err := s.Store.nextSweep(s.Config.Treasury, s.Config.MinAmount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, _ = s.Store.DB.Exec("UPDATE sweep_jobs SET last_error=?,updated_at=? WHERE id=?", err.Error(), unixNow(), j.ID)
		}
	}()
	if j.Status == "review" {
		return errors.New("sweep requires operator review; automatic sends paused")
	}
	return s.process(ctx, j, safe)
}

func (s *Sweeper) safeBlock(ctx context.Context) (uint64, error) {
	l, err := s.RPC.block(ctx, "latest")
	if err != nil {
		return 0, err
	}
	f, err := s.RPC.block(ctx, "finalized")
	if err != nil {
		return 0, err
	}
	n, _ := hexUint(l.Number)
	final, _ := hexUint(f.Number)
	if n+1 < s.Store.Config.Confirmations {
		return 0, nil
	}
	safe := n - s.Store.Config.Confirmations + 1
	if final < safe {
		safe = final
	}
	return safe, nil
}

func (s *Sweeper) quantity(ctx context.Context, method string, params []any) (*big.Int, error) {
	var value string
	if err := s.RPC.call(ctx, method, params, &value); err != nil {
		return nil, err
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(value, "0x"), 16)
	if !strings.HasPrefix(value, "0x") || !ok || n.Sign() < 0 || n.Cmp(uint256Max) > 0 {
		return nil, errors.New("invalid RPC numeric value")
	}
	return n, nil
}

func tokenData(selector, address string, amount *big.Int) string {
	data := "0x" + selector + strings.Repeat("0", 24) + strings.ToLower(address[2:])
	if amount != nil {
		data += fmt.Sprintf("%064x", amount)
	}
	return data
}

func (s *Sweeper) process(ctx context.Context, j SweepJob, safe uint64) error {
	var pending SweepTransaction
	err := s.Store.DB.QueryRow("SELECT id,job_id,kind,tx_hash,raw_tx,sender,nonce,amount,state,created_at FROM sweep_transactions WHERE job_id=? AND state='prepared' ORDER BY id LIMIT 1", j.ID).Scan(&pending.ID, &pending.JobID, &pending.Kind, &pending.Hash, &pending.Raw, &pending.Sender, &pending.Nonce, &pending.Amount, &pending.State, &pending.Created)
	if err == nil {
		return s.settleOrBroadcast(ctx, j, pending, safe)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	i, err := s.Store.Get(j.InvoiceID)
	if err != nil {
		return err
	}
	if i.Address == s.GasAddress || i.Address == j.Treasury {
		return s.review(j, "deposit address conflicts with gas/main wallet")
	}
	key, err := privateAt(s.Seed, i.Index)
	if err != nil {
		return err
	}
	defer key.D.SetInt64(0)
	if !strings.EqualFold(crypto.PubkeyToAddress(key.PublicKey).Hex(), i.Address) {
		return s.review(j, "derived private key does not match deposit address")
	}
	amount, err := parseAmount(j.Amount)
	if err != nil {
		return err
	}
	balance, err := s.quantity(ctx, "eth_call", []any{map[string]string{"to": s.Store.Config.Token, "data": tokenData("70a08231", i.Address, nil)}, hexNumber(safe)})
	if err != nil {
		return err
	}
	if balance.Cmp(amount) < 0 {
		return errors.New("confirmed USDT balance below queued amount; no transfer sent")
	}
	data := tokenData("a9059cbb", j.Treasury, amount)
	result, err := s.quantity(ctx, "eth_call", []any{map[string]string{"to": s.Store.Config.Token, "from": i.Address, "data": data}, "latest"})
	if err != nil {
		return err
	}
	if result.Cmp(big.NewInt(1)) != 0 {
		return s.review(j, "token transfer simulation returned false")
	}
	price, err := s.quantity(ctx, "eth_gasPrice", []any{})
	if err != nil {
		return err
	}
	if price.Sign() <= 0 || price.Cmp(s.Config.MaxGasPrice) > 0 {
		return errors.New("gas price outside configured limit; waiting")
	}
	budget := new(big.Int).Mul(new(big.Int).SetUint64(s.Config.TokenGas), price)
	budget.Mul(budget, big.NewInt(125))
	budget.Div(budget, big.NewInt(100))
	native, err := s.quantity(ctx, "eth_getBalance", []any{i.Address, "latest"})
	if err != nil {
		return err
	}
	if native.Cmp(budget) < 0 {
		needed := new(big.Int).Sub(budget, native)
		rows, err := s.Store.DB.Query("SELECT amount FROM sweep_transactions WHERE job_id=? AND kind='gas'", j.ID)
		if err != nil {
			return err
		}
		total := new(big.Int).Set(needed)
		for rows.Next() {
			var v string
			if err = rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			n, ok := new(big.Int).SetString(v, 10)
			if !ok {
				rows.Close()
				return errors.New("invalid recorded gas amount")
			}
			total.Add(total, n)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if total.Cmp(s.Config.MaxTopup) > 0 {
			return errors.New("gas top-up budget exceeded; waiting for lower gas price or operator adjustment")
		}
		gasBalance, err := s.quantity(ctx, "eth_getBalance", []any{s.GasAddress, "latest"})
		if err != nil {
			return err
		}
		cost := new(big.Int).Add(needed, new(big.Int).Mul(big.NewInt(21000), price))
		if gasBalance.Cmp(cost) < 0 {
			return errors.New("gas wallet has insufficient BNB; fund the configured gas wallet")
		}
		return s.prepare(ctx, j, "gas", s.GasKey, i.Address, needed, nil, 21000, price)
	}
	bytes, err := hex.DecodeString(strings.TrimPrefix(data, "0x"))
	if err != nil {
		return err
	}
	return s.prepare(ctx, j, "token", key, s.Store.Config.Token, new(big.Int), bytes, s.Config.TokenGas, price)
}

func (s *Sweeper) prepare(ctx context.Context, j SweepJob, kind string, key *ecdsa.PrivateKey, to string, value *big.Int, data []byte, gas uint64, price *big.Int) error {
	sender := strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
	pending, err := s.quantity(ctx, "eth_getTransactionCount", []any{sender, "pending"})
	if err != nil {
		return err
	}
	latest, err := s.quantity(ctx, "eth_getTransactionCount", []any{sender, "latest"})
	if err != nil {
		return err
	}
	if !pending.IsUint64() || pending.Cmp(latest) != 0 {
		return errors.New("sender has an unrelated pending transaction; waiting")
	}
	dest := common.HexToAddress(to)
	tx := types.NewTx(&types.LegacyTx{Nonce: pending.Uint64(), To: &dest, Value: value, Gas: gas, GasPrice: price, Data: data})
	signed, err := types.SignTx(tx, types.NewEIP155Signer(new(big.Int).SetUint64(s.Store.Config.ChainID)), key)
	if err != nil {
		return err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return err
	}
	amount := value.String()
	if kind == "token" {
		n, e := parseAmount(j.Amount)
		if e != nil {
			return e
		}
		amount = n.String()
	}
	// Persist the exact signed bytes BEFORE touching the network. A timeout,
	// process death or restart can only resend this same hash/nonce.
	dbtx, err := s.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer dbtx.Rollback()
	_, err = dbtx.Exec("INSERT INTO sweep_transactions(job_id,kind,tx_hash,raw_tx,sender,nonce,amount,state,created_at) VALUES(?,?,?,?,?,?,?,'prepared',?)", j.ID, kind, strings.ToLower(signed.Hash().Hex()), "0x"+hex.EncodeToString(raw), sender, pending.Uint64(), amount, unixNow())
	if err != nil {
		return err
	}
	status := "gas_pending"
	if kind == "token" {
		status = "transfer_pending"
	}
	if _, err = dbtx.Exec("UPDATE sweep_jobs SET status=?,last_error='',updated_at=? WHERE id=?", status, unixNow(), j.ID); err != nil {
		return err
	}
	return dbtx.Commit()
}

type sweepReceipt struct {
	Hash      string        `json:"transactionHash"`
	Block     string        `json:"blockNumber"`
	BlockHash string        `json:"blockHash"`
	Status    string        `json:"status"`
	Logs      []TransferLog `json:"logs"`
}

func (s *Sweeper) settleOrBroadcast(ctx context.Context, j SweepJob, t SweepTransaction, safe uint64) error {
	var raw json.RawMessage
	if err := s.RPC.call(ctx, "eth_getTransactionReceipt", []any{t.Hash}, &raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("missing transaction receipt result")
	}
	if string(raw) == "null" {
		nonce, err := s.quantity(ctx, "eth_getTransactionCount", []any{t.Sender, hexNumber(safe)})
		if err != nil {
			return err
		}
		if nonce.Cmp(new(big.Int).SetUint64(t.Nonce)) > 0 {
			return s.review(j, "sender nonce consumed but persisted transaction is missing; no replacement sent")
		}
		var hash string
		if err = s.RPC.call(ctx, "eth_sendRawTransaction", []any{t.Raw}, &hash); err != nil {
			return errors.New("broadcast not acknowledged; same signed transaction retained for retry")
		}
		if !strings.EqualFold(hash, t.Hash) {
			return s.review(j, "RPC broadcast returned an unexpected hash")
		}
		return nil
	}
	var receipt sweepReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return err
	}
	if !strings.EqualFold(receipt.Hash, t.Hash) {
		return s.review(j, "receipt transaction hash mismatch")
	}
	n, err := hexUint(receipt.Block)
	if err != nil {
		return err
	}
	if n > safe {
		return nil
	}
	b, err := s.RPC.block(ctx, hexNumber(n))
	if err != nil {
		return err
	}
	bn, _ := hexUint(b.Number)
	if bn != n || !strings.EqualFold(b.Hash, receipt.BlockHash) {
		return errors.New("sweep receipt is not canonical; waiting")
	}
	if receipt.Status != "0x1" {
		return s.review(j, "signed transaction reverted; automatic sends paused")
	}
	if t.Kind == "token" {
		total := new(big.Int)
		for _, log := range receipt.Logs {
			if log.Removed || !strings.EqualFold(log.Address, s.Store.Config.Token) || len(log.Topics) != 3 || !strings.EqualFold(log.Topics[0], transferTopic) {
				continue
			}
			if !strings.EqualFold(log.Topics[1], "0x"+strings.Repeat("0", 24)+t.Sender[2:]) || !strings.EqualFold(log.Topics[2], "0x"+strings.Repeat("0", 24)+j.Treasury[2:]) {
				continue
			}
			if !hashPattern.MatchString(log.Data) {
				return s.review(j, "invalid outgoing transfer log")
			}
			n, ok := new(big.Int).SetString(log.Data[2:], 16)
			if !ok {
				return errors.New("invalid outgoing amount")
			}
			total.Add(total, n)
		}
		expected, ok := new(big.Int).SetString(t.Amount, 10)
		if !ok || total.Cmp(expected) != 0 {
			return s.review(j, "successful receipt did not transfer the expected USDT to main wallet")
		}
	}
	dbtx, err := s.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer dbtx.Rollback()
	if _, err = dbtx.Exec("UPDATE sweep_transactions SET state='confirmed' WHERE id=? AND state='prepared'", t.ID); err != nil {
		return err
	}
	status := "queued"
	if t.Kind == "token" {
		status = "completed"
	}
	if _, err = dbtx.Exec("UPDATE sweep_jobs SET status=?,last_error='',updated_at=? WHERE id=?", status, unixNow(), j.ID); err != nil {
		return err
	}
	return dbtx.Commit()
}

func (s *Sweeper) review(j SweepJob, message string) error {
	_, err := s.Store.DB.Exec("UPDATE sweep_jobs SET status='review',last_error=?,updated_at=? WHERE id=?", message, unixNow(), j.ID)
	if err != nil {
		return err
	}
	return errors.New(message)
}
