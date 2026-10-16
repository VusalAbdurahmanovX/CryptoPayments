package main

import (
	"database/sql"
	"errors"
	"math/big"
)

type SweepJob struct {
	ID           string             `json:"id"`
	InvoiceID    string             `json:"invoice_id"`
	Treasury     string             `json:"main_wallet_address"`
	Amount       string             `json:"amount"`
	Status       string             `json:"status"`
	LastError    string             `json:"last_error"`
	Created      int64              `json:"created_at"`
	Updated      int64              `json:"updated_at"`
	Transactions []SweepTransaction `json:"transactions"`
}

type SweepTransaction struct {
	ID      int64  `json:"id"`
	JobID   string `json:"-"`
	Kind    string `json:"kind"`
	Hash    string `json:"tx_hash"`
	Sender  string `json:"from_address"`
	Nonce   uint64 `json:"nonce"`
	Amount  string `json:"amount_units"`
	State   string `json:"state"`
	Created int64  `json:"created_at"`
	Raw     string `json:"-"`
}

func (s *Store) initSweepStore() error {
	_, err := s.DB.Exec(`
CREATE TABLE IF NOT EXISTS sweep_jobs(
 id TEXT PRIMARY KEY, invoice_id TEXT NOT NULL REFERENCES invoices(id), treasury TEXT NOT NULL,
 amount TEXT NOT NULL,status TEXT NOT NULL,last_error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS sweep_one_active ON sweep_jobs(invoice_id) WHERE status!='completed';
CREATE TABLE IF NOT EXISTS sweep_transactions(
 id INTEGER PRIMARY KEY,job_id TEXT NOT NULL REFERENCES sweep_jobs(id),kind TEXT NOT NULL,
 tx_hash TEXT NOT NULL UNIQUE,raw_tx TEXT NOT NULL,sender TEXT NOT NULL,nonce INTEGER NOT NULL,
 amount TEXT NOT NULL,state TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS sweep_nonce ON sweep_transactions(sender,nonce);
CREATE TABLE IF NOT EXISTS sweep_worker(
 id INTEGER PRIMARY KEY CHECK(id=1),treasury TEXT NOT NULL,gas_address TEXT NOT NULL,last_tick INTEGER NOT NULL,last_error TEXT NOT NULL);`)
	return err
}

const jobColumns = "id,invoice_id,treasury,amount,status,last_error,created_at,updated_at"

func scanJob(row rowScanner) (SweepJob, error) {
	var j SweepJob
	err := row.Scan(&j.ID, &j.InvoiceID, &j.Treasury, &j.Amount, &j.Status, &j.LastError, &j.Created, &j.Updated)
	j.Transactions = []SweepTransaction{}
	return j, err
}

func (s *Store) sweepJobs(id string) ([]SweepJob, error) {
	rows, err := s.DB.Query("SELECT "+jobColumns+" FROM sweep_jobs WHERE invoice_id=? ORDER BY created_at,id", id)
	if err != nil {
		return nil, err
	}
	jobs := []SweepJob{}
	for rows.Next() {
		j, e := scanJob(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for n := range jobs {
		rows, err = s.DB.Query("SELECT id,job_id,kind,tx_hash,sender,nonce,amount,state,created_at FROM sweep_transactions WHERE job_id=? ORDER BY id", jobs[n].ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t SweepTransaction
			if err = rows.Scan(&t.ID, &t.JobID, &t.Kind, &t.Hash, &t.Sender, &t.Nonce, &t.Amount, &t.State, &t.Created); err != nil {
				rows.Close()
				return nil, err
			}
			jobs[n].Transactions = append(jobs[n].Transactions, t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

func (s *Store) workerStatus() (map[string]any, error) {
	var treasury, gas, lastError string
	var last int64
	err := s.DB.QueryRow("SELECT treasury,gas_address,last_tick,last_error FROM sweep_worker WHERE id=1").Scan(&treasury, &gas, &last, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{"running": false}, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"running": unixNow()-last < 120, "last_tick": last, "last_error": lastError, "main_wallet_address": treasury, "gas_wallet_address": gas}, nil
}

func (s *Store) nextSweep(treasury string, min *big.Int) (SweepJob, error) {
	// One active job globally keeps the dedicated gas wallet's nonce serialized.
	j, err := scanJob(s.DB.QueryRow("SELECT " + jobColumns + " FROM sweep_jobs WHERE status!='completed' ORDER BY created_at,id LIMIT 1"))
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return j, err
	}
	rows, err := s.DB.Query("SELECT id FROM invoices WHERE received!='0' ORDER BY created_at,id")
	if err != nil {
		return j, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e := rows.Scan(&id); e != nil {
			rows.Close()
			return j, e
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return j, err
	}
	for _, id := range ids {
		i, e := s.Get(id)
		if e != nil {
			return j, e
		}
		total := new(big.Int)
		for _, p := range i.Transfers {
			n, e := parseAmount(p.Amount)
			if e != nil {
				return j, e
			}
			total.Add(total, n)
		}
		jobs, e := s.sweepJobs(id)
		if e != nil {
			return j, e
		}
		for _, job := range jobs {
			if job.Status == "completed" {
				n, e := parseAmount(job.Amount)
				if e != nil {
					return j, e
				}
				total.Sub(total, n)
			}
		}
		if total.Sign() <= 0 || total.Cmp(min) < 0 {
			continue
		}
		if i.Address == treasury {
			return j, errors.New("main wallet cannot be a deposit address")
		}
		jobID, e := newID("swp_")
		if e != nil {
			return j, e
		}
		now := unixNow()
		_, err = s.DB.Exec("INSERT INTO sweep_jobs(id,invoice_id,treasury,amount,status,created_at,updated_at) VALUES(?,?,?,?,'queued',?,?)", jobID, id, treasury, formatAmount(total), now, now)
		if err != nil {
			return j, err
		}
		return scanJob(s.DB.QueryRow("SELECT "+jobColumns+" FROM sweep_jobs WHERE id=?", jobID))
	}
	return j, sql.ErrNoRows
}
