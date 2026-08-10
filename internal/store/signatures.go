package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Signature is a reusable email signature with three format variants. The
// variant used at compose time is picked by the active compose mode (see
// Variant); ApplyMode ("always" | "first") decides whether it is auto-inserted
// on replies.
type Signature struct {
	ID        int64
	Account   string
	Name      string
	TextPlain string
	HTML      string
	Markdown  string
	ApplyMode string // "always" (every message) | "first" (new messages only, not replies)
}

// Variant returns the signature text matching a compose mode: html, markdown,
// or (default) plain text.
func (sig Signature) Variant(mode string) string {
	switch mode {
	case "html":
		return sig.HTML
	case "markdown":
		return sig.Markdown
	default:
		return sig.TextPlain
	}
}

// SignatureApplies reports whether a signature with the given apply mode should
// be auto-inserted for a compose of the given kind. "first" (first message
// only) excludes replies; every other case applies.
func SignatureApplies(applyMode, kind string) bool {
	if applyMode == "first" && (kind == "reply" || kind == "reply_all") {
		return false
	}
	return true
}

const signatureCols = `id, account, name, text_plain, html, markdown, apply_mode`

func scanSignature(sc interface{ Scan(...any) error }) (Signature, error) {
	var sig Signature
	err := sc.Scan(&sig.ID, &sig.Account, &sig.Name, &sig.TextPlain, &sig.HTML, &sig.Markdown, &sig.ApplyMode)
	return sig, err
}

// ListSignatures returns every signature, newest first.
func (s *Store) ListSignatures() ([]Signature, error) {
	rows, err := s.DB.Query(`SELECT ` + signatureCols + ` FROM signatures ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Signature
	for rows.Next() {
		sig, err := scanSignature(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sig)
	}
	return out, rows.Err()
}

// SignatureByID returns one signature, or nil if absent.
func (s *Store) SignatureByID(id int64) (*Signature, error) {
	sig, err := scanSignature(s.DB.QueryRow(`SELECT `+signatureCols+` FROM signatures WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sig, nil
}

// UpsertSignature inserts (ID == 0) or updates a signature, setting sig.ID on
// insert. ApplyMode defaults to "always" if not "first".
func (s *Store) UpsertSignature(sig *Signature) error {
	if strings.TrimSpace(sig.Name) == "" {
		return errors.New("signature: name required")
	}
	if sig.ApplyMode != "first" {
		sig.ApplyMode = "always"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if sig.ID == 0 {
		res, err := s.DB.Exec(`INSERT INTO signatures
			(account, name, text_plain, html, markdown, apply_mode, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			sig.Account, sig.Name, sig.TextPlain, sig.HTML, sig.Markdown, sig.ApplyMode, now, now)
		if err != nil {
			return err
		}
		sig.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := s.DB.Exec(`UPDATE signatures SET
		account = ?, name = ?, text_plain = ?, html = ?, markdown = ?, apply_mode = ?, updated_at = ?
		WHERE id = ?`,
		sig.Account, sig.Name, sig.TextPlain, sig.HTML, sig.Markdown, sig.ApplyMode, now, sig.ID)
	return err
}

// DeleteSignature removes a signature; identity links to it cascade away.
func (s *Store) DeleteSignature(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM signatures WHERE id = ?`, id)
	return err
}

// sigAddrKey normalizes an identity address to the link table key.
func sigAddrKey(address string) string { return strings.ToLower(strings.TrimSpace(address)) }

// SetIdentitySignature links an identity (bare email) to a signature. sigID 0
// removes the link (back to the "no signature" default).
func (s *Store) SetIdentitySignature(address string, sigID int64) error {
	key := sigAddrKey(address)
	if key == "" {
		return errors.New("signature: address required")
	}
	if sigID == 0 {
		_, err := s.DB.Exec(`DELETE FROM identity_signatures WHERE address = ?`, key)
		return err
	}
	_, err := s.DB.Exec(`INSERT INTO identity_signatures (address, signature_id) VALUES (?, ?)
		ON CONFLICT(address) DO UPDATE SET signature_id = excluded.signature_id`, key, sigID)
	return err
}

// IdentitySignatureID returns the signature id linked to an identity, or 0.
func (s *Store) IdentitySignatureID(address string) (int64, error) {
	var id int64
	err := s.DB.QueryRow(`SELECT signature_id FROM identity_signatures WHERE address = ?`, sigAddrKey(address)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// IdentitySignatureMap returns address -> signature id for every linked
// identity (addresses are lowercased bare emails).
func (s *Store) IdentitySignatureMap() (map[string]int64, error) {
	rows, err := s.DB.Query(`SELECT address, signature_id FROM identity_signatures`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var addr string
		var id int64
		if err := rows.Scan(&addr, &id); err != nil {
			return nil, err
		}
		out[addr] = id
	}
	return out, rows.Err()
}
