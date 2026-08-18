// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

// No card data is stored here, ever. Stripe hosts the checkout; this service
// only ever sees an email, a customer id and a subscription id.
const schema = `
CREATE TABLE IF NOT EXISTS licences (
  id                     TEXT PRIMARY KEY,
  email                  TEXT NOT NULL,
  plan                   TEXT NOT NULL,
  key                    TEXT NOT NULL,
  stripe_customer_id     TEXT NOT NULL DEFAULT '',
  stripe_subscription_id TEXT,
  status                 TEXT NOT NULL DEFAULT 'active',
  created_at             INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS licences_email_idx    ON licences(email);
CREATE INDEX IF NOT EXISTS licences_customer_idx ON licences(stripe_customer_id);

CREATE TABLE IF NOT EXISTS stripe_events (
  id      TEXT PRIMARY KEY,
  seen_at INTEGER NOT NULL
);
`

type licence struct {
	ID           string
	Email        string
	Plan         string
	Key          string
	CustomerID   string
	Subscription string
	Status       string
	CreatedAt    int64
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// Same trade as the main repo: one connection, no SQLITE_BUSY dance. This
	// service handles a handful of writes a week.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// claimEvent records a Stripe event id and reports whether we are the first to
// see it. Called inside the issuing transaction so a webhook replay can never
// issue a second licence.
func claimEvent(tx *sql.Tx, id string) (bool, error) {
	res, err := tx.Exec(`INSERT OR IGNORE INTO stripe_events (id, seen_at) VALUES (?, ?)`, id, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func insertLicence(tx *sql.Tx, l licence) error {
	var sub any
	if l.Subscription != "" {
		sub = l.Subscription
	}
	_, err := tx.Exec(
		`INSERT INTO licences (id, email, plan, key, stripe_customer_id, stripe_subscription_id, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'active', ?)`,
		l.ID, l.Email, l.Plan, l.Key, l.CustomerID, sub, l.CreatedAt)
	return err
}

// licenceBySubscription finds the licence a Stripe subscription pays for.
func licenceBySubscription(tx *sql.Tx, subscriptionID string) (licence, bool, error) {
	var l licence
	if subscriptionID == "" {
		return l, false, nil
	}
	err := tx.QueryRow(
		`SELECT id, email, plan, key, stripe_customer_id, COALESCE(stripe_subscription_id, ''), status, created_at
		 FROM licences WHERE stripe_subscription_id = ?`, subscriptionID).
		Scan(&l.ID, &l.Email, &l.Plan, &l.Key, &l.CustomerID, &l.Subscription, &l.Status, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return l, false, nil
	}
	return l, err == nil, err
}

// activateLicence clears a lapsed mark without touching the key. A paid invoice
// means the customer is current, whether or not it moved the expiry.
func activateLicence(tx *sql.Tx, id string) error {
	_, err := tx.Exec(`UPDATE licences SET status = 'active' WHERE id = ?`, id)
	return err
}

// renewLicence swaps in a freshly signed key for a licence that already exists,
// keeping its id. Updating in place rather than inserting is what keeps
// /retrieve returning one current key per purchase instead of a pile of expired
// ones, and it flips a lapsed licence back to active now that it is paid for.
func renewLicence(tx *sql.Tx, id, key string, issuedAt int64) error {
	_, err := tx.Exec(
		`UPDATE licences SET key = ?, status = 'active', created_at = ? WHERE id = ?`,
		key, issuedAt, id)
	return err
}

// licencesByEmail returns every licence issued to an address, newest first.
// Comparison is case-insensitive because people type their email by hand.
func licencesByEmail(db *sql.DB, email string) ([]licence, error) {
	rows, err := db.Query(
		`SELECT id, email, plan, key, stripe_customer_id, COALESCE(stripe_subscription_id, ''), status, created_at
		 FROM licences WHERE email = ? COLLATE NOCASE ORDER BY created_at DESC`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []licence
	for rows.Next() {
		var l licence
		if err := rows.Scan(&l.ID, &l.Email, &l.Plan, &l.Key, &l.CustomerID, &l.Subscription, &l.Status, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// lapse marks licences as lapsed for record keeping only. Verification in pro/
// is offline by design: a lapsed licence keeps working until it expires on its
// own terms. There is no revocation channel and there will not be one.
func lapse(db *sql.DB, customerID, subscriptionID string) error {
	if customerID == "" && subscriptionID == "" {
		return nil
	}
	_, err := db.Exec(
		`UPDATE licences SET status = 'lapsed'
		 WHERE plan = 'annual' AND status = 'active'
		   AND ((? <> '' AND stripe_customer_id = ?) OR (? <> '' AND stripe_subscription_id = ?))`,
		customerID, customerID, subscriptionID, subscriptionID)
	return err
}
