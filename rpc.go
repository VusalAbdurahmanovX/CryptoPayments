package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const transferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

type RPC struct {
	URL      string
	Client   *http.Client
	sequence atomic.Uint64
}
type Block struct {
	Number    string `json:"number"`
	Hash      string `json:"hash"`
	Timestamp string `json:"timestamp"`
}
type TransferLog struct {
	Address     string   `json:"address"`
	Topics      []string `json:"topics"`
	Data        string   `json:"data"`
	BlockNumber string   `json:"blockNumber"`
	BlockHash   string   `json:"blockHash"`
	TxHash      string   `json:"transactionHash"`
	LogIndex    string   `json:"logIndex"`
	Removed     bool     `json:"removed"`
}

func newRPC(url string) *RPC {
	return &RPC{URL: url, Client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (r *RPC) call(ctx context.Context, method string, params any, out any) error {
	id := r.sequence.Add(1)
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", r.URL, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid RPC request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return errors.New("RPC connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("RPC HTTP %d", resp.StatusCode)
	}
	var result struct {
		ID     uint64          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return errors.New("invalid RPC response")
	}
	if result.ID != id {
		return errors.New("RPC response id mismatch")
	}
	if result.Error != nil {
		return fmt.Errorf("RPC %s error %d", method, result.Error.Code)
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = result.Result
		return nil
	}
	if len(result.Result) == 0 || string(result.Result) == "null" {
		return fmt.Errorf("RPC %s returned no result", method)
	}
	return json.Unmarshal(result.Result, out)
}

func hexUint(s string) (uint64, error) {
	if !strings.HasPrefix(s, "0x") || len(s) < 3 {
		return 0, errors.New("invalid hex quantity")
	}
	return strconv.ParseUint(s[2:], 16, 64)
}
func hexNumber(n uint64) string { return fmt.Sprintf("0x%x", n) }

func (r *RPC) block(ctx context.Context, tag string) (Block, error) {
	var b Block
	err := r.call(ctx, "eth_getBlockByNumber", []any{tag, false}, &b)
	if err != nil {
		return b, err
	}
	if _, err = hexUint(b.Number); err != nil {
		return b, err
	}
	if _, err = hexUint(b.Timestamp); err != nil {
		return b, err
	}
	if !hashPattern.MatchString(b.Hash) {
		return b, errors.New("invalid block hash")
	}
	return b, nil
}

func (r *RPC) validate(ctx context.Context, c Config) error {
	var chain string
	if err := r.call(ctx, "eth_chainId", []any{}, &chain); err != nil {
		return err
	}
	id, err := hexUint(chain)
	if err != nil || id != c.ChainID {
		return errors.New("RPC chain ID does not match configuration")
	}
	var decimals string
	if err := r.call(ctx, "eth_call", []any{map[string]string{"to": c.Token, "data": "0x313ce567"}, "latest"}, &decimals); err != nil {
		return err
	}
	d, ok := new(big.Int).SetString(strings.TrimPrefix(decimals, "0x"), 16)
	if !ok || d.Cmp(big.NewInt(18)) != 0 {
		return errors.New("token must expose decimals() = 18")
	}
	_, err = r.block(ctx, "finalized")
	return err
}

func (r *RPC) transfers(ctx context.Context, c Config, address string, from, to uint64) ([]TransferLog, error) {
	var logs []TransferLog
	filter := map[string]any{"address": c.Token, "fromBlock": hexNumber(from), "toBlock": hexNumber(to), "topics": []any{transferTopic, nil, "0x" + strings.Repeat("0", 24) + strings.ToLower(address[2:])}}
	err := r.call(ctx, "eth_getLogs", []any{filter}, &logs)
	return logs, err
}
