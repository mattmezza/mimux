package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// OutAttachment is one file queued with a scheduled/delayed message. Data is
// the raw file bytes; encoding/json base64-encodes it in the stored blob. Kept
// here (not mail.OutAttachment) because mail imports store, not the reverse.
type OutAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Outbox is one queued outgoing message: the full compose payload plus its
// scheduled send time and delivery status. Undo-send and scheduled-send both
// land here; the scheduler drains due rows.
type Outbox struct {
	ID          int64
	Account     string
	From        string
	To          string
	Cc          string
	Bcc         string
	Subject     string
	Body        string
	Mode        string
	InReplyTo   string
	References  string
	Attachments []OutAttachment
	SendAt      time.Time
	Status      string
	Error       string
	CreatedAt   time.Time
}

const outboxCols = `id, account, from_addr, to_addresses, cc_addresses, bcc_addresses,
	subject, body, mode, in_reply_to, refs, attachments, send_at, status, error, created_at`

func scanOutbox(sc interface{ Scan(...any) error }) (*Outbox, error) {
	o := &Outbox{}
	var atts, sendAt, createdAt string
	if err := sc.Scan(&o.ID, &o.Account, &o.From, &o.To, &o.Cc, &o.Bcc,
		&o.Subject, &o.Body, &o.Mode, &o.InReplyTo, &o.References, &atts,
		&sendAt, &o.Status, &o.Error, &createdAt); err != nil {
		return nil, err
	}
	if atts != "" {
		_ = json.Unmarshal([]byte(atts), &o.Attachments)
	}
	o.SendAt, _ = time.Parse(time.RFC3339, sendAt)
	o.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return o, nil
}

// EnqueueOutbox inserts a queued message, writing the assigned id back.
func (s *Store) EnqueueOutbox(o *Outbox) error {
	atts := ""
	if len(o.Attachments) > 0 {
		b, err := json.Marshal(o.Attachments)
		if err != nil {
			return err
		}
		atts = string(b)
	}
	if o.Status == "" {
		o.Status = "queued"
	}
	res, err := s.DB.Exec(`
		INSERT INTO outbox (account, from_addr, to_addresses, cc_addresses, bcc_addresses,
			subject, body, mode, in_reply_to, refs, attachments, send_at, status, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.Account, o.From, o.To, o.Cc, o.Bcc, o.Subject, o.Body, o.Mode, o.InReplyTo,
		o.References, atts, o.SendAt.UTC().Format(time.RFC3339), o.Status, o.Error,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	o.ID, err = res.LastInsertId()
	return err
}

// DueOutbox returns queued rows whose send time has arrived (send_at <= now).
func (s *Store) DueOutbox(now time.Time) ([]Outbox, error) {
	return s.queryOutbox(`SELECT `+outboxCols+` FROM outbox
		WHERE status = 'queued' AND send_at <= ? ORDER BY send_at`, now.UTC().Format(time.RFC3339))
}

// ListScheduled returns still-queued rows, soonest first — for the Drafts page
// "Scheduled" section.
func (s *Store) ListScheduled() ([]Outbox, error) {
	return s.queryOutbox(`SELECT ` + outboxCols + ` FROM outbox WHERE status = 'queued' ORDER BY send_at`)
}

// ListFailed returns failed rows, most-recently-created first — the Outgoing
// page "Failed" section.
func (s *Store) ListFailed() ([]Outbox, error) {
	return s.queryOutbox(`SELECT ` + outboxCols + ` FROM outbox WHERE status = 'failed' ORDER BY created_at DESC`)
}

// ListRecentSent returns rows sent at/after `since`, newest first — the quiet
// "Recently sent" reassurance section. send_at is used as the sent-time proxy
// (there is no separate sent_at column; the scheduler delivers at send_at).
func (s *Store) ListRecentSent(since time.Time) ([]Outbox, error) {
	return s.queryOutbox(`SELECT `+outboxCols+` FROM outbox
		WHERE status = 'sent' AND send_at >= ? ORDER BY send_at DESC`, since.UTC().Format(time.RFC3339))
}

// RetryOutbox re-queues a failed row for immediate send (status -> queued,
// send_at -> now). Returns false if the row is no longer failed (raced).
func (s *Store) RetryOutbox(id int64) (bool, error) {
	res, err := s.DB.Exec(`UPDATE outbox SET status = 'queued', send_at = ?, error = '' WHERE id = ? AND status = 'failed'`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteOutbox removes a row entirely — the Failed-section Delete action.
func (s *Store) DeleteOutbox(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM outbox WHERE id = ?`, id)
	return err
}

func (s *Store) queryOutbox(q string, args ...any) ([]Outbox, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Outbox
	for rows.Next() {
		o, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

func (s *Store) OutboxByID(id int64) (*Outbox, error) {
	o, err := scanOutbox(s.DB.QueryRow(`SELECT `+outboxCols+` FROM outbox WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}

// ClaimOutbox flips a queued row to "sending", returning true only if this
// call won the row — guards the scheduler against sending the same message
// twice if a send outlasts the tick interval.
func (s *Store) ClaimOutbox(id int64) (bool, error) {
	res, err := s.DB.Exec(`UPDATE outbox SET status = 'sending' WHERE id = ? AND status = 'queued'`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RecoverSending rescues rows stranded in "sending" — claimed by the scheduler
// but never marked sent or failed because the process died mid-send (air
// restart, deploy, crash). Without this they are retried by nobody and shown by
// nothing (DueOutbox/ListScheduled/ListFailed all skip "sending").
//
// They land in "failed", not "queued", on purpose: the process may have died
// *after* SMTP accepted the message, and each send generates a fresh
// Message-ID (mail.BuildMessage) that is never stored, so a re-send is
// undetectable as a duplicate by us or the recipient. At-most-once by default;
// the user sees the row in Outgoing → Failed and picks Retry or Delete.
//
// Called once at scheduler start, where "sending" unambiguously means stranded:
// single process, single scheduler, so nothing is in flight yet.
// NOTE: boot-time only. A send that hangs while the process lives stays
// "sending" until the next restart — add a send deadline if that ever bites.
func (s *Store) RecoverSending() (int64, error) {
	res, err := s.DB.Exec(`UPDATE outbox SET status = 'failed', error = ? WHERE status = 'sending'`,
		"interrupted mid-send by a restart — it may already have gone out, check Sent before retrying")
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) MarkOutboxSent(id int64) error {
	_, err := s.DB.Exec(`UPDATE outbox SET status = 'sent', error = '' WHERE id = ?`, id)
	return err
}

func (s *Store) MarkOutboxFailed(id int64, msg string) error {
	_, err := s.DB.Exec(`UPDATE outbox SET status = 'failed', error = ? WHERE id = ?`, msg, id)
	return err
}

// CancelOutbox cancels a queued message (undo-send or the Drafts-page Cancel).
// Returns false if the row is no longer queued (already sending/sent) — i.e.
// the undo window closed.
func (s *Store) CancelOutbox(id int64) (bool, error) {
	res, err := s.DB.Exec(`UPDATE outbox SET status = 'cancelled' WHERE id = ? AND status = 'queued'`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
