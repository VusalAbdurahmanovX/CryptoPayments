package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	_ "github.com/mattn/go-sqlite3"
)

var errConflict = errors.New("order_id already exists with a different amount")
var errNotFound = errors.New("invoice not found")

type Invoice struct {
	ID            string    `json:"id"`
	OrderID       string    `json:"order_id"`
	Address       string    `json:"address"`
	Index         uint32    `json:"address_index"`
	Path          string    `json:"derivation_path"`
	Amount        string    `json:"amount"`
	Received      string    `json:"received_amount"`
	Status        string    `json:"status"`
	IsPaid        bool      `json:"is_paid"`
	CreatedAt     int64     `json:"created_at"`
	ExpiresAt     int64     `json:"expires_at"`
	NextBlock     uint64    `json:"next_scan_block"`
	ChainID       uint64    `json:"chain_id"`
	Network       string    `json:"network"`
	Token         string    `json:"token_contract"`
	Asset         string    `json:"asset"`
	Confirmations uint64    `json:"required_confirmations"`
	Transfers     []Payment `json:"transfers"`
}

type Payment struct {
	TxHash    string `json:"tx_hash"`
	LogIndex  uint64 `json:"log_index"`
	Block     uint64 `json:"block_number"`
	BlockHash string `json:"block_hash"`
	Time      int64  `json:"block_time"`
	Amount    string `json:"amount"`
}

type Store struct {
	DB     *sql.DB
	Config Config
	Xpub   *hdkeychain.ExtendedKey
}

func openStore(c Config) (*Store, error) {
	abs, err := filepath.Abs(c.Database)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	db, err := sql.Open("sqlite3", u.String()+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000&_foreign_keys=on&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	xpub, err := readXpub(c.Xpub)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db, Config: c, Xpub: xpub}
	success := false
	defer func() {
		if !success {
			db.Close()
		}
	}()
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS invoices(
 id TEXT PRIMARY KEY, order_id TEXT NOT NULL UNIQUE, address TEXT NOT NULL UNIQUE,
 address_index INTEGER NOT NULL UNIQUE, amount TEXT NOT NULL, received TEXT NOT NULL DEFAULT '0',
 status TEXT NOT NULL DEFAULT 'pending', created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
 next_block INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS transfers(
 tx_hash TEXT NOT NULL, log_index INTEGER NOT NULL, invoice_id TEXT NOT NULL REFERENCES invoices(id),
 block_number INTEGER NOT NULL, block_hash TEXT NOT NULL, block_time INTEGER NOT NULL, amount TEXT NOT NULL,
 PRIMARY KEY(tx_hash,log_index));
CREATE TABLE IF NOT EXISTS events(
 id TEXT PRIMARY KEY, invoice_id TEXT NOT NULL REFERENCES invoices(id), payload BLOB NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt INTEGER NOT NULL DEFAULT 0,
 delivered_at INTEGER, created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS invoices_cursor ON invoices(next_block);
CREATE INDEX IF NOT EXISTS transfers_invoice ON transfers(invoice_id);
CREATE INDEX IF NOT EXISTS events_pending ON events(delivered_at,next_attempt);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	// A database must never silently switch wallet, chain or accepted token.
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, err
	}
	defer tx.Rollback()
	for key, value := range map[string]string{"schema": "1", "xpub": c.Xpub, "chain_id": fmt.Sprint(c.ChainID), "token": strings.ToLower(c.Token)} {
		var existing string
		e := tx.QueryRow("SELECT value FROM metadata WHERE key=?", key).Scan(&existing)
		if errors.Is(e, sql.ErrNoRows) {
			_, e = tx.Exec("INSERT INTO metadata(key,value) VALUES(?,?)", key, value)
		} else if e == nil && existing != value {
			e = fmt.Errorf("database %s mismatch; use a separate database", key)
		}
		if e != nil {
			return nil, e
		}
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO metadata(key,value) VALUES('next_index','0')"); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if err = os.Chmod(abs, 0600); err != nil {
		db.Close()
		return nil, err
	}
	success = true
	return s, nil
}

const invoiceColumns = "id,order_id,address,address_index,amount,received,status,created_at,expires_at,next_block"

type rowScanner interface{ Scan(...any) error }

func (s *Store) scanInvoice(row rowScanner) (Invoice, error) {
	var i Invoice
	err := row.Scan(&i.ID, &i.OrderID, &i.Address, &i.Index, &i.Amount, &i.Received, &i.Status, &i.CreatedAt, &i.ExpiresAt, &i.NextBlock)
	if errors.Is(err, sql.ErrNoRows) {
		return i, errNotFound
	}
	i.ChainID = s.Config.ChainID
	i.Token = s.Config.Token
	i.Asset = "USDT"
	i.Network = "bsc-mainnet"
	if i.ChainID == 97 {
		i.Network = "bsc-testnet"
		i.Asset = "TEST-USDT"
	}
	i.Confirmations = s.Config.Confirmations
	i.Path = fmt.Sprintf("%s/%d", walletPath, i.Index)
	i.Transfers = []Payment{}
	i.IsPaid = fullyPaid(i.Status)
	return i, err
}

func fullyPaid(status string) bool {
	return status == "paid" || status == "overpaid" || status == "paid_late" || status == "overpaid_late"
}

func newID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

func (s *Store) Create(order string, amount *big.Int, start uint64, now int64) (Invoice, bool, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return Invoice{}, false, err
	}
	defer tx.Rollback()
	existing, err := s.scanInvoice(tx.QueryRow("SELECT "+invoiceColumns+" FROM invoices WHERE order_id=?", order))
	if err == nil {
		if existing.Amount != formatAmount(amount) {
			return Invoice{}, false, errConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, errNotFound) {
		return Invoice{}, false, err
	}
	var index uint32
	if err = tx.QueryRow("SELECT value FROM metadata WHERE key='next_index'").Scan(&index); err != nil {
		return Invoice{}, false, err
	}
	address, err := depositAddress(s.Xpub, index)
	if err != nil {
		return Invoice{}, false, err
	}
	id, err := newID("inv_")
	if err != nil {
		return Invoice{}, false, err
	}
	_, err = tx.Exec("INSERT INTO invoices(id,order_id,address,address_index,amount,created_at,expires_at,next_block) VALUES(?,?,?,?,?,?,?,?)", id, order, address, index, formatAmount(amount), now, now+s.Config.InvoiceTTL, start)
	if err != nil {
		return Invoice{}, false, err
	}
	if _, err = tx.Exec("UPDATE metadata SET value=? WHERE key='next_index'", index+1); err != nil {
		return Invoice{}, false, err
	}
	i, err := s.scanInvoice(tx.QueryRow("SELECT "+invoiceColumns+" FROM invoices WHERE id=?", id))
	if err != nil {
		return i, false, err
	}
	if err = s.enqueue(tx, "invoice.created", i, now); err != nil {
		return i, false, err
	}
	err = tx.Commit()
	return i, true, err
}

func (s *Store) Get(id string) (Invoice, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return Invoice{}, err
	}
	defer tx.Rollback()
	i, err := s.scanInvoice(tx.QueryRow("SELECT "+invoiceColumns+" FROM invoices WHERE id=?", id))
	if err != nil {
		return i, err
	}
	rows, err := tx.Query("SELECT tx_hash,log_index,block_number,block_hash,block_time,amount FROM transfers WHERE invoice_id=? ORDER BY block_number,log_index", id)
	if err != nil {
		return i, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Payment
		if err = rows.Scan(&p.TxHash, &p.LogIndex, &p.Block, &p.BlockHash, &p.Time, &p.Amount); err != nil {
			return i, err
		}
		i.Transfers = append(i.Transfers, p)
	}
	return i, rows.Err()
}

func (s *Store) due(target uint64) ([]Invoice, error) {
	rows, err := s.DB.Query("SELECT "+invoiceColumns+" FROM invoices WHERE next_block<=? ORDER BY next_block,id LIMIT 100", target)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Invoice{}
	for rows.Next() {
		i, e := s.scanInvoice(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func paymentStatus(expected, received *big.Int, completion, expires, now int64) string {
	cmp := received.Cmp(expected)
	if cmp >= 0 {
		status := "paid"
		if cmp > 0 {
			status = "overpaid"
		}
		if completion > expires {
			status += "_late"
		}
		return status
	}
	if now >= expires {
		return "expired"
	}
	if received.Sign() > 0 {
		return "partially_paid"
	}
	return "pending"
}

func (s *Store) apply(i Invoice, payments []Payment, next uint64, now int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := s.scanInvoice(tx.QueryRow("SELECT "+invoiceColumns+" FROM invoices WHERE id=?", i.ID))
	if err != nil {
		return err
	}
	if current.NextBlock != i.NextBlock {
		return errors.New("scan cursor changed")
	}
	for _, p := range payments {
		_, err = tx.Exec("INSERT OR IGNORE INTO transfers(tx_hash,log_index,invoice_id,block_number,block_hash,block_time,amount) VALUES(?,?,?,?,?,?,?)", p.TxHash, p.LogIndex, i.ID, p.Block, p.BlockHash, p.Time, p.Amount)
		if err != nil {
			return err
		}
	}
	rows, err := tx.Query("SELECT tx_hash,log_index,block_number,block_hash,block_time,amount FROM transfers WHERE invoice_id=? ORDER BY block_number,log_index", i.ID)
	if err != nil {
		return err
	}
	all := []Payment{}
	for rows.Next() {
		var p Payment
		if err = rows.Scan(&p.TxHash, &p.LogIndex, &p.Block, &p.BlockHash, &p.Time, &p.Amount); err != nil {
			rows.Close()
			return err
		}
		all = append(all, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	expected, err := parseAmount(current.Amount)
	if err != nil {
		return err
	}
	total := new(big.Int)
	var completion int64
	for _, p := range all {
		n, e := parseAmount(p.Amount)
		if e != nil {
			return e
		}
		total.Add(total, n)
		if completion == 0 && total.Cmp(expected) >= 0 {
			completion = p.Time
		}
	}
	status := paymentStatus(expected, total, completion, current.ExpiresAt, now)
	_, err = tx.Exec("UPDATE invoices SET received=?,status=?,next_block=? WHERE id=?", formatAmount(total), status, next, i.ID)
	if err != nil {
		return err
	}
	if status != current.Status || formatAmount(total) != current.Received {
		current.Received = formatAmount(total)
		current.Status = status
		current.IsPaid = fullyPaid(status)
		current.NextBlock = next
		current.Transfers = all
		if err = s.enqueue(tx, "invoice.updated", current, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) expire(now int64) error {
	rows, err := s.DB.Query("SELECT "+invoiceColumns+" FROM invoices WHERE expires_at<=? AND status IN ('pending','partially_paid')", now)
	if err != nil {
		return err
	}
	var invoices []Invoice
	for rows.Next() {
		i, e := s.scanInvoice(rows)
		if e != nil {
			rows.Close()
			return e
		}
		invoices = append(invoices, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, i := range invoices {
		if err = s.apply(i, nil, i.NextBlock, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) enqueue(tx *sql.Tx, event string, i Invoice, now int64) error {
	if s.Config.WebhookURL == "" {
		return nil
	}
	id, err := newID("evt_")
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"id": id, "type": event, "created_at": now, "invoice": i})
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO events(id,invoice_id,payload,created_at) VALUES(?,?,?,?)", id, i.ID, body, now)
	return err
}

func unixNow() int64 { return time.Now().UTC().Unix() }
