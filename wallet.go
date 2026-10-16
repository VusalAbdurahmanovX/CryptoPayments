package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"golang.org/x/crypto/sha3"
)

const walletPath = "m/44'/60'/0'/0"

func publicBranch(seed []byte) (*hdkeychain.ExtendedKey, error) {
	k, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		return nil, err
	}
	defer k.Zero()
	for _, index := range []uint32{44 + hdkeychain.HardenedKeyStart, 60 + hdkeychain.HardenedKeyStart, hdkeychain.HardenedKeyStart, 0} {
		next, e := k.Derive(index)
		if e != nil {
			return nil, e
		}
		k.Zero()
		k = next
	}
	defer k.Zero()
	pub, err := k.Neuter()
	if err != nil {
		return nil, err
	}
	// Neuter shares backing slices with the private key. Serialize a public-only
	// copy before clearing the private branch.
	return hdkeychain.NewKeyFromString(pub.String())
}

func readXpub(s string) (*hdkeychain.ExtendedKey, error) {
	k, err := hdkeychain.NewKeyFromString(s)
	if err != nil {
		return nil, errors.New("invalid WALLET_XPUB")
	}
	if k.IsPrivate() || k.Depth() != 4 || k.ChildIndex() != 0 || !k.IsForNet(&chaincfg.MainNetParams) {
		return nil, errors.New("WALLET_XPUB must be the public external branch at m/44'/60'/0'/0")
	}
	return k, nil
}

func depositAddress(k *hdkeychain.ExtendedKey, index uint32) (string, error) {
	if index >= hdkeychain.HardenedKeyStart {
		return "", errors.New("address index exhausted")
	}
	child, err := k.Derive(index)
	if err != nil {
		return "", err
	}
	defer child.Zero()
	pub, err := child.ECPubKey()
	if err != nil {
		return "", err
	}
	h := sha3.NewLegacyKeccak256()
	h.Write(pub.SerializeUncompressed()[1:])
	return "0x" + hex.EncodeToString(h.Sum(nil)[12:]), nil
}

// Only this offline command handles the seed. The API process only receives xpub.
func initWallet(path string) error {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	defer func() {
		for i := range seed {
			seed[i] = 0
		}
	}()
	xpub, err := publicBranch(seed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, hex.EncodeToString(seed))
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
	fmt.Printf("WALLET_XPUB=%s\n", xpub.String())
	fmt.Fprintf(os.Stderr, "Seed saved to %s (0600). Back it up offline with the database. Path: %s/<index>. This is raw BIP32 seed hex, NOT a mnemonic.\n", path, walletPath)
	return nil
}
