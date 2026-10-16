package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

type Scanner struct {
	Store       *Store
	RPC         *RPC
	mu          sync.RWMutex
	LastSuccess int64
	LastError   string
	SafeBlock   uint64
}

func (s *Scanner) Tick(ctx context.Context) (err error) {
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.LastError = err.Error()
		} else {
			s.LastSuccess = unixNow()
			s.LastError = ""
		}
	}()
	c := s.Store.Config
	latest, err := s.RPC.block(ctx, "latest")
	if err != nil {
		return err
	}
	head, _ := hexUint(latest.Number)
	final, err := s.RPC.block(ctx, "finalized")
	if err != nil {
		return err
	}
	finalized, _ := hexUint(final.Number)
	if head < c.Confirmations {
		return nil
	}
	target := head - c.Confirmations + 1
	if finalized < target {
		target = finalized
	}
	s.mu.Lock()
	s.SafeBlock = target
	s.mu.Unlock()
	invoices, err := s.Store.due(target)
	if err != nil {
		return err
	}
	// Per-invoice cursors mean new invoices and process restarts cannot lose blocks.
	var firstError error
	for _, i := range invoices {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		end := target
		if end-i.NextBlock >= c.BatchBlocks {
			end = i.NextBlock + c.BatchBlocks - 1
		}
		if e := s.scanInvoice(ctx, i, end); e != nil && firstError == nil {
			firstError = fmt.Errorf("invoice %s: %w", i.ID, e)
		}
	}
	if err = s.Store.expire(unixNow()); err != nil {
		return err
	}
	return firstError
}

func (s *Scanner) scanInvoice(ctx context.Context, i Invoice, end uint64) error {
	logs, err := s.RPC.transfers(ctx, s.Store.Config, i.Address, i.NextBlock, end)
	if err != nil {
		return err
	}
	blocks := map[uint64]Block{}
	payments := []Payment{}
	for _, log := range logs {
		if log.Removed {
			return errors.New("removed log returned in finalized range")
		}
		if !strings.EqualFold(log.Address, s.Store.Config.Token) || len(log.Topics) != 3 || !strings.EqualFold(log.Topics[0], transferTopic) {
			return errors.New("RPC returned unexpected token event")
		}
		if !hashPattern.MatchString(log.Topics[1]) || !strings.EqualFold(log.Topics[2], "0x"+strings.Repeat("0", 24)+i.Address[2:]) {
			return errors.New("RPC returned unexpected recipient")
		}
		block, err := hexUint(log.BlockNumber)
		if err != nil {
			return err
		}
		if block < i.NextBlock || block > end {
			return errors.New("log outside requested block range")
		}
		index, err := hexUint(log.LogIndex)
		if err != nil {
			return err
		}
		if !hashPattern.MatchString(log.TxHash) || !hashPattern.MatchString(log.BlockHash) || !hashPattern.MatchString(log.Data) {
			return errors.New("invalid transfer log encoding")
		}
		n, ok := new(big.Int).SetString(log.Data[2:], 16)
		if !ok {
			return errors.New("invalid transfer value")
		}
		if n.Sign() == 0 {
			continue
		}
		b, ok := blocks[block]
		if !ok {
			b, err = s.RPC.block(ctx, hexNumber(block))
			if err != nil {
				return err
			}
			bn, _ := hexUint(b.Number)
			if bn != block {
				return errors.New("block number mismatch")
			}
			blocks[block] = b
		}
		if !strings.EqualFold(b.Hash, log.BlockHash) {
			return errors.New("noncanonical transfer block")
		}
		stamp, err := hexUint(b.Timestamp)
		if err != nil || stamp > 1<<63-1 {
			return errors.New("invalid transfer timestamp")
		}
		payments = append(payments, Payment{TxHash: strings.ToLower(log.TxHash), LogIndex: index, Block: block, BlockHash: strings.ToLower(log.BlockHash), Time: int64(stamp), Amount: formatAmount(n)})
	}
	return s.Store.apply(i, payments, end+1, unixNow())
}

func (s *Scanner) Run(ctx context.Context) {
	for {
		_ = s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.Store.Config.Poll):
		}
	}
}
