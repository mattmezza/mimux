// SPDX-License-Identifier: AGPL-3.0-only
package store

import "database/sql"

// SenderAllowsExternal reports whether the sender is trusted to load external
// content automatically.
func (s *Store) SenderAllowsExternal(address string) (bool, error) {
	var allow int
	err := s.DB.QueryRow(`SELECT allow_external FROM sender_prefs WHERE address = ?`, address).Scan(&allow)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return allow == 1, nil
}

// SetSenderAllowsExternal upserts the sender's external-content preference.
func (s *Store) SetSenderAllowsExternal(address string, allow bool) error {
	_, err := s.DB.Exec(`INSERT INTO sender_prefs (address, allow_external) VALUES (?, ?)
		ON CONFLICT(address) DO UPDATE SET allow_external = excluded.allow_external`,
		address, b2i(allow))
	return err
}
