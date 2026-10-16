package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const mainnetUSDT = "0x55d398326f99059ff775485246999027b3197955"

var addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
var hashPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)

type Config struct {
	Listen, Database, APIKey, Xpub, RPCURL, Token, WebhookURL, WebhookSecret string
	ChainID, Confirmations, BatchBlocks                                      uint64
	Poll                                                                     time.Duration
	InvoiceTTL                                                               int64
}

func loadConfig() (Config, error) {
	c := Config{Listen: env("LISTEN_ADDR", ":8080"), Database: env("DATABASE_PATH", "/data/payments.sqlite"), APIKey: os.Getenv("API_KEY"), Xpub: os.Getenv("WALLET_XPUB"), RPCURL: os.Getenv("RPC_URL"), Token: strings.ToLower(os.Getenv("TOKEN_CONTRACT")), WebhookURL: os.Getenv("WEBHOOK_URL"), WebhookSecret: os.Getenv("WEBHOOK_SECRET")}
	for _, field := range []struct {
		name string
		def  uint64
		dest *uint64
	}{
		{"CHAIN_ID", 97, &c.ChainID}, {"CONFIRMATIONS", 15, &c.Confirmations}, {"SCAN_BATCH_BLOCKS", 500, &c.BatchBlocks},
	} {
		n, err := strconv.ParseUint(env(field.name, strconv.FormatUint(field.def, 10)), 10, 64)
		if err != nil || n == 0 {
			return c, fmt.Errorf("invalid %s", field.name)
		}
		*field.dest = n
	}
	if c.ChainID != 56 && c.ChainID != 97 {
		return c, errors.New("CHAIN_ID must be 56 (mainnet) or 97 (testnet)")
	}
	if c.Confirmations > 10000 || c.BatchBlocks > 1000 {
		return c, errors.New("confirmations or scan batch too large")
	}
	if c.ChainID == 56 {
		if c.Token == "" {
			c.Token = mainnetUSDT
		}
		if c.Token != mainnetUSDT {
			return c, errors.New("mainnet accepts only the configured BSC USDT contract")
		}
	}
	if !addressPattern.MatchString(c.Token) || c.Token == "0x"+strings.Repeat("0", 40) {
		return c, errors.New("TOKEN_CONTRACT must be set to the test token contract on testnet")
	}
	if len(c.APIKey) < 32 {
		return c, errors.New("API_KEY must be at least 32 characters")
	}
	if _, err := readXpub(c.Xpub); err != nil {
		return c, err
	}
	if err := validURL(c.RPCURL, c.ChainID == 97); err != nil {
		return c, fmt.Errorf("RPC_URL: %w", err)
	}
	if c.WebhookURL != "" {
		if len(c.WebhookSecret) < 32 {
			return c, errors.New("WEBHOOK_SECRET must be at least 32 characters")
		}
		if err := validURL(c.WebhookURL, c.ChainID == 97); err != nil {
			return c, fmt.Errorf("WEBHOOK_URL: %w", err)
		}
	}
	poll, err := strconv.Atoi(env("POLL_SECONDS", "10"))
	if err != nil || poll < 1 || poll > 3600 {
		return c, errors.New("invalid POLL_SECONDS")
	}
	c.Poll = time.Duration(poll) * time.Second
	c.InvoiceTTL, err = strconv.ParseInt(env("INVOICE_TTL_SECONDS", "3600"), 10, 64)
	if err != nil || c.InvoiceTTL < 1 || c.InvoiceTTL > 604800 {
		return c, errors.New("invalid INVOICE_TTL_SECONDS")
	}
	return c, nil
}

func env(k, def string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return def
}
func validURL(s string, allowHTTP bool) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
		return errors.New("valid HTTPS URL required (HTTP allowed only on testnet)")
	}
	return nil
}
