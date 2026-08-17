// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"errors"
	"strings"
	"time"
)

// Template is a reusable message body the user can drop into a compose.
type Template struct {
	ID   int64
	Name string
	Body string
}

// ListTemplates returns every template, sorted by name (case-insensitive).
func (s *Store) ListTemplates() ([]Template, error) {
	rows, err := s.DB.Query(`SELECT id, name, body FROM templates ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Template
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.ID, &t.Name, &t.Body); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpsertTemplate inserts (ID == 0) or updates a template, setting t.ID on insert.
func (s *Store) UpsertTemplate(t *Template) error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("template: name required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if t.ID == 0 {
		res, err := s.DB.Exec(`INSERT INTO templates (name, body, created_at, updated_at) VALUES (?,?,?,?)`,
			t.Name, t.Body, now, now)
		if err != nil {
			return err
		}
		t.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := s.DB.Exec(`UPDATE templates SET name = ?, body = ?, updated_at = ? WHERE id = ?`,
		t.Name, t.Body, now, t.ID)
	return err
}

// DeleteTemplate removes a template.
func (s *Store) DeleteTemplate(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM templates WHERE id = ?`, id)
	return err
}
