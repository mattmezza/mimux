// SPDX-License-Identifier: AGPL-3.0-only
package store

import "database/sql"

// PushSub is one browser-per-device Web Push subscription (see migration 0160).
// Label is a human hint for the Settings device list, taken from the User-Agent
// at subscribe time — the endpoint itself is an opaque vendor URL.
type PushSub struct {
	Endpoint  string
	P256dh    string
	Auth      string
	Label     string
	CreatedAt string
}

// SavePushSub stores (or refreshes) a subscription. Re-subscribing the same
// endpoint updates its keys rather than duplicating the device.
func (s *Store) SavePushSub(p PushSub) error {
	_, err := s.DB.Exec(`
		INSERT INTO push_subs (endpoint, p256dh, auth, label) VALUES (?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh,
			auth = excluded.auth, label = excluded.label`,
		p.Endpoint, p.P256dh, p.Auth, p.Label)
	return err
}

// ListPushSubs returns every registered device, newest first.
func (s *Store) ListPushSubs() ([]PushSub, error) {
	rows, err := s.DB.Query(`SELECT endpoint, p256dh, auth, label, created_at
		FROM push_subs ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PushSub
	for rows.Next() {
		var p PushSub
		if err := rows.Scan(&p.Endpoint, &p.P256dh, &p.Auth, &p.Label, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePushSub forgets a device: the user removed it in Settings, signed out
// on it, or its push service answered 404/410 ("this subscription is gone").
func (s *Store) DeletePushSub(endpoint string) error {
	_, err := s.DB.Exec(`DELETE FROM push_subs WHERE endpoint = ?`, endpoint)
	return err
}

// VAPIDKeys returns the stored key pair, or empty strings when none has been
// generated yet. Generation lives in the mail package (it owns the webpush
// dependency); the store only remembers.
func (s *Store) VAPIDKeys() (public, private string) {
	err := s.DB.QueryRow(`SELECT public, private FROM push_keys WHERE id = 1`).Scan(&public, &private)
	if err != nil && err != sql.ErrNoRows {
		return "", ""
	}
	return public, private
}

// SaveVAPIDKeys persists the generated key pair. Rotating it invalidates every
// existing subscription, so this only ever runs once (id is pinned to 1).
func (s *Store) SaveVAPIDKeys(public, private string) error {
	_, err := s.DB.Exec(`INSERT INTO push_keys (id, public, private) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET public = excluded.public, private = excluded.private`,
		public, private)
	return err
}
