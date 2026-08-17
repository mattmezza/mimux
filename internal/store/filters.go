// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/mattmezza/mimux/internal/filter"
)

// ListRules returns every rule (any account, global included), ordered by
// position. This is what the filters management page shows.
func (s *Store) ListRules() ([]filter.Rule, error) {
	rows, err := s.DB.Query(`SELECT id, account, name, position, enabled, conditions, actions FROM filter_rules ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []filter.Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RulesForAccount returns enabled rules that apply to account: global rules
// (Account == "") plus rules scoped to that account, in position order. The
// sync engine calls this when a message arrives.
func (s *Store) RulesForAccount(account string) ([]filter.Rule, error) {
	rows, err := s.DB.Query(`SELECT id, account, name, position, enabled, conditions, actions FROM filter_rules
		WHERE enabled = 1 AND (account = '' OR account = ?) ORDER BY position, id`, account)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []filter.Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetRule(id int64) (*filter.Rule, error) {
	row := s.DB.QueryRow(`SELECT id, account, name, position, enabled, conditions, actions FROM filter_rules WHERE id = ?`, id)
	r, err := scanRule(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateRule inserts r, appending it to the end of the position order. r.ID
// and r.Position are set to the stored values on return.
func (s *Store) CreateRule(r *filter.Rule) error {
	cond, err := json.Marshal(r.Conditions)
	if err != nil {
		return err
	}
	act, err := json.Marshal(r.Actions)
	if err != nil {
		return err
	}
	var maxPos sql.NullInt64
	if err := s.DB.QueryRow(`SELECT MAX(position) FROM filter_rules`).Scan(&maxPos); err != nil {
		return err
	}
	pos := int(maxPos.Int64) + 1
	res, err := s.DB.Exec(`INSERT INTO filter_rules (account, name, position, enabled, conditions, actions) VALUES (?, ?, ?, ?, ?, ?)`,
		r.Account, r.Name, pos, boolToInt(r.Enabled), string(cond), string(act))
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	r.ID, r.Position = id, pos
	return nil
}

// UpdateRule replaces name/account/conditions/actions/enabled for r.ID.
// Position is untouched here; use ReorderRules for that.
func (s *Store) UpdateRule(r filter.Rule) error {
	cond, err := json.Marshal(r.Conditions)
	if err != nil {
		return err
	}
	act, err := json.Marshal(r.Actions)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`UPDATE filter_rules SET account = ?, name = ?, enabled = ?, conditions = ?, actions = ? WHERE id = ?`,
		r.Account, r.Name, boolToInt(r.Enabled), string(cond), string(act), r.ID)
	return err
}

func (s *Store) DeleteRule(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM filter_rules WHERE id = ?`, id)
	return err
}

// ToggleRule flips a rule's enabled flag.
func (s *Store) ToggleRule(id int64) error {
	_, err := s.DB.Exec(`UPDATE filter_rules SET enabled = 1 - enabled WHERE id = ?`, id)
	return err
}

// ReorderRules sets position = index in ids for each rule, in one
// transaction. Unknown ids are ignored; ids not present keep their position.
func (s *Store) ReorderRules(ids []int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE filter_rules SET position = ? WHERE id = ?`, i, id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// rowScanner covers both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRule(row rowScanner) (filter.Rule, error) {
	var r filter.Rule
	var enabled int
	var cond, act string
	if err := row.Scan(&r.ID, &r.Account, &r.Name, &r.Position, &enabled, &cond, &act); err != nil {
		return filter.Rule{}, err
	}
	r.Enabled = enabled != 0
	if err := json.Unmarshal([]byte(cond), &r.Conditions); err != nil {
		return filter.Rule{}, fmt.Errorf("filter_rules %d: conditions: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(act), &r.Actions); err != nil {
		return filter.Rule{}, fmt.Errorf("filter_rules %d: actions: %w", r.ID, err)
	}
	return r, nil
}
