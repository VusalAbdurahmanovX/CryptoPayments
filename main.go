package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 {
		if os.Args[1] == "healthcheck" {
			client := &http.Client{Timeout: 3 * time.Second}
			resp, err := client.Get("http://127.0.0.1:8080/healthz")
			if err != nil {
				os.Exit(1)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				os.Exit(1)
			}
			return
		}
		if os.Args[1] == "wallet-init" && len(os.Args) == 3 {
			if err := initWallet(os.Args[2]); err != nil {
				log.Fatal(err)
			}
			return
		}
		log.Fatal("usage: cryptopayments [wallet-init /secure/path/wallet.seed | healthcheck]")
	}
	c, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	s, err := openStore(c)
	if err != nil {
		log.Fatal(err)
	}
	defer s.DB.Close()
	rpc := newRPC(c.RPCURL)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err = rpc.validate(ctx, c); err != nil {
		log.Fatalf("blockchain configuration validation failed: %v", err)
	}
	scanner := &Scanner{Store: s, RPC: rpc}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() { defer workers.Done(); scanner.Run(ctx) }()
	go func() { defer workers.Done(); newDispatcher(s).Run(ctx) }()
	api := &API{Store: s, Scanner: scanner, RPC: rpc}
	server := &http.Server{Addr: c.Listen, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Printf("CryptoPayments API listening on %s; chain_id=%d; watch-only; token=%s", c.Listen, c.ChainID, c.Token)
	serverError := make(chan error, 1)
	go func() { serverError <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-serverError:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP server failed: %v", err)
		}
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close()
	}
	workers.Wait()
}
