package main

import (
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/ethereum/go-ethereum/crypto"
)

type SweepConfig struct {
	Treasury                         string
	SeedFile, GasKeyFile             string
	TokenGas                         uint64
	MaxGasPrice, MaxTopup, MinAmount *big.Int
	Poll                             time.Duration
}

func initGasWallet(path string) error {
	k, err := crypto.GenerateKey()
	if err != nil {
		return err
	}
	defer k.D.SetInt64(0)
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	data := crypto.FromECDSA(k)
	defer func() {
		for i := range data {
			data[i] = 0
		}
	}()
	_, err = fmt.Fprintln(f, hex.EncodeToString(data))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Printf("GAS_WALLET_ADDRESS=%s\n", strings.ToLower(crypto.PubkeyToAddress(k.PublicKey).Hex()))
	return nil
}

func loadSweepConfig(c Config) (SweepConfig, error) {
	s := SweepConfig{Treasury: strings.ToLower(os.Getenv("MAIN_WALLET_ADDRESS")), SeedFile: os.Getenv("SWEEP_SEED_FILE"), GasKeyFile: os.Getenv("GAS_PRIVATE_KEY_FILE")}
	if os.Getenv("AUTO_SWEEP_ENABLED") != "true" {
		return s, errors.New("AUTO_SWEEP_ENABLED=true is required to start the signer")
	}
	if !addressPattern.MatchString(s.Treasury) || s.Treasury == "0x"+strings.Repeat("0", 40) || s.Treasury == c.Token {
		return s, errors.New("invalid MAIN_WALLET_ADDRESS")
	}
	if s.SeedFile == "" || s.GasKeyFile == "" {
		return s, errors.New("SWEEP_SEED_FILE and GAS_PRIVATE_KEY_FILE are required")
	}
	var err error
	s.TokenGas, err = strconv.ParseUint(env("SWEEP_TOKEN_GAS_LIMIT", "100000"), 10, 64)
	if err != nil || s.TokenGas < 21000 || s.TokenGas > 500000 {
		return s, errors.New("invalid SWEEP_TOKEN_GAS_LIMIT")
	}
	for _, v := range []struct {
		name, def string
		dest      **big.Int
	}{
		{"SWEEP_MAX_GAS_PRICE_WEI", "10000000000", &s.MaxGasPrice},
		{"SWEEP_MAX_TOPUP_WEI", "2000000000000000", &s.MaxTopup},
	} {
		n, ok := new(big.Int).SetString(env(v.name, v.def), 10)
		if !ok || n.Sign() <= 0 || n.Cmp(uint256Max) > 0 {
			return s, errors.New("invalid " + v.name)
		}
		*v.dest = n
	}
	s.MinAmount = new(big.Int)
	if v := env("SWEEP_MIN_AMOUNT", "0"); v != "0" {
		s.MinAmount, err = parseAmount(v)
		if err != nil {
			return s, errors.New("invalid SWEEP_MIN_AMOUNT")
		}
	}
	seconds, err := strconv.Atoi(env("SWEEP_POLL_SECONDS", "10"))
	if err != nil || seconds < 1 || seconds > 3600 {
		return s, errors.New("invalid SWEEP_POLL_SECONDS")
	}
	s.Poll = time.Duration(seconds) * time.Second
	return s, nil
}

func readSecret(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open signer secret file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1024 {
		return nil, errors.New("signer secrets must be regular files, at most 1024 bytes, with permissions 0600 or 0400")
	}
	b := make([]byte, info.Size())
	if _, err := f.ReadAt(b, 0); err != nil {
		return nil, err
	}
	data, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"))
	for i := range b {
		b[i] = 0
	}
	if err != nil || len(data) != 32 {
		return nil, errors.New("signer secret must be exactly 32 bytes encoded as hex")
	}
	return data, nil
}

func privateAt(seed []byte, index uint32) (*ecdsa.PrivateKey, error) {
	if index >= hdkeychain.HardenedKeyStart {
		return nil, errors.New("invalid child index")
	}
	k, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		return nil, err
	}
	for _, part := range []uint32{44 + hdkeychain.HardenedKeyStart, 60 + hdkeychain.HardenedKeyStart, hdkeychain.HardenedKeyStart, 0, index} {
		next, e := k.Derive(part)
		k.Zero()
		if e != nil {
			return nil, e
		}
		k = next
	}
	defer k.Zero()
	priv, err := k.ECPrivKey()
	if err != nil {
		return nil, err
	}
	defer priv.Zero()
	serialized := priv.Serialize()
	defer func() {
		for i := range serialized {
			serialized[i] = 0
		}
	}()
	return crypto.ToECDSA(serialized)
}
