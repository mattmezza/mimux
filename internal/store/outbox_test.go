// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"testing"
	"time"
)

func TestOutboxCRUD(t *testing.T) {
	s := open(t)

	if due, err := s.DueOutbox(time.Now()); err != nil || len(due) != 0 {
		t.Fatalf("fresh db: due=%v err=%v", due, err)
	}

	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)

	dueRow := &Outbox{Account: "work", To: "a@x", Subject: "now", Body: "b", Mode: "plain",
		SendAt: past, Attachments: []OutAttachment{{Filename: "f.txt", Data: []byte("hi")}}}
	if err := s.EnqueueOutbox(dueRow); err != nil {
		t.Fatal(err)
	}
	if dueRow.ID == 0 {
		t.Fatal("expected assigned id")
	}
	later := &Outbox{Account: "work", To: "b@x", Subject: "later", SendAt: future}
	if err := s.EnqueueOutbox(later); err != nil {
		t.Fatal(err)
	}

	// Due selection: only the past row, and its attachment blob round-trips.
	due, err := s.DueOutbox(time.Now())
	if err != nil || len(due) != 1 {
		t.Fatalf("DueOutbox = %v (%d), %v; want 1", due, len(due), err)
	}
	if due[0].ID != dueRow.ID {
		t.Errorf("due id = %d, want %d", due[0].ID, dueRow.ID)
	}
	if len(due[0].Attachments) != 1 || string(due[0].Attachments[0].Data) != "hi" {
		t.Errorf("attachments round-trip = %+v", due[0].Attachments)
	}

	// Both are visible as scheduled until acted on.
	if sc, err := s.ListScheduled(); err != nil || len(sc) != 2 {
		t.Fatalf("ListScheduled = %v, %v; want 2", sc, err)
	}

	// Claim wins once; a second claim on the same row loses.
	if ok, err := s.ClaimOutbox(dueRow.ID); err != nil || !ok {
		t.Fatalf("first claim = %v, %v; want true", ok, err)
	}
	if ok, _ := s.ClaimOutbox(dueRow.ID); ok {
		t.Error("second claim should lose")
	}
	if err := s.MarkOutboxSent(dueRow.ID); err != nil {
		t.Fatal(err)
	}

	// Cancel the future row; it drops out of scheduled and can't be re-cancelled.
	if ok, err := s.CancelOutbox(later.ID); err != nil || !ok {
		t.Fatalf("cancel = %v, %v; want true", ok, err)
	}
	if ok, _ := s.CancelOutbox(later.ID); ok {
		t.Error("cancel of already-cancelled row should return false")
	}
	if sc, err := s.ListScheduled(); err != nil || len(sc) != 0 {
		t.Fatalf("ListScheduled after cancel/sent = %v, %v; want 0", sc, err)
	}

	// dueRow was marked sent above: it shows up in the 24h recently-sent window.
	if rs, err := s.ListRecentSent(time.Now().Add(-24 * time.Hour)); err != nil || len(rs) != 1 {
		t.Fatalf("ListRecentSent = %v, %v; want 1", rs, err)
	}
	if rs, _ := s.ListRecentSent(time.Now().Add(time.Hour)); len(rs) != 0 {
		t.Errorf("ListRecentSent with future cutoff should be empty, got %d", len(rs))
	}
}

// A row claimed but never marked (process died mid-send) is invisible to every
// list and never retried — RecoverSending is what unsticks it.
func TestRecoverSending(t *testing.T) {
	s := open(t)
	past := time.Now().Add(-time.Minute)
	stuck := &Outbox{Account: "work", To: "a@x", Subject: "stranded", SendAt: past}
	ok := &Outbox{Account: "work", To: "b@x", Subject: "fine", SendAt: past}
	for _, o := range []*Outbox{stuck, ok} {
		if err := s.EnqueueOutbox(o); err != nil {
			t.Fatal(err)
		}
	}
	if claimed, err := s.ClaimOutbox(stuck.ID); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	// The black hole: not due, not scheduled, not failed.
	if due, _ := s.DueOutbox(time.Now()); len(due) != 1 || due[0].ID != ok.ID {
		t.Fatalf("stranded row should not be due, got %v", due)
	}
	if sc, _ := s.ListScheduled(); len(sc) != 1 || sc[0].ID != ok.ID {
		t.Fatalf("ListScheduled = %v; want only the unclaimed row", sc)
	}
	if fl, _ := s.ListFailed(); len(fl) != 0 {
		t.Fatalf("ListFailed = %v; want 0 before recovery", fl)
	}

	if n, err := s.RecoverSending(); err != nil || n != 1 {
		t.Fatalf("RecoverSending = %d, %v; want 1", n, err)
	}
	fl, err := s.ListFailed()
	if err != nil || len(fl) != 1 || fl[0].ID != stuck.ID || fl[0].Error == "" {
		t.Fatalf("ListFailed after recovery = %v, %v; want the stranded row with an error", fl, err)
	}
	// Recovered rows go through the normal Retry path.
	if retried, err := s.RetryOutbox(stuck.ID); err != nil || !retried {
		t.Fatalf("retry of recovered row = %v, %v; want true", retried, err)
	}

	// The normal queued -> sending -> sent flow is untouched by recovery.
	if claimed, _ := s.ClaimOutbox(ok.ID); !claimed {
		t.Fatal("claim of queued row failed")
	}
	if err := s.MarkOutboxSent(ok.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RecoverSending(); err != nil || n != 0 {
		t.Fatalf("second RecoverSending = %d, %v; want 0", n, err)
	}
	if rs, _ := s.ListRecentSent(time.Now().Add(-time.Hour)); len(rs) != 1 || rs[0].ID != ok.ID {
		t.Fatalf("ListRecentSent = %v; want the sent row still sent", rs)
	}
}

func TestOutboxRetryAndDelete(t *testing.T) {
	s := open(t)
	row := &Outbox{Account: "work", To: "a@x", Subject: "boom", SendAt: time.Now().Add(time.Hour)}
	if err := s.EnqueueOutbox(row); err != nil {
		t.Fatal(err)
	}
	// Retry only bites on failed rows.
	if ok, _ := s.RetryOutbox(row.ID); ok {
		t.Error("retry of a queued row should be a no-op")
	}
	if err := s.MarkOutboxFailed(row.ID, "smtp exploded"); err != nil {
		t.Fatal(err)
	}
	if fl, err := s.ListFailed(); err != nil || len(fl) != 1 || fl[0].Error != "smtp exploded" {
		t.Fatalf("ListFailed = %v, %v; want 1 with error", fl, err)
	}
	// Retry re-queues (status->queued, error cleared) and makes it due now.
	if ok, err := s.RetryOutbox(row.ID); err != nil || !ok {
		t.Fatalf("retry = %v, %v; want true", ok, err)
	}
	if due, _ := s.DueOutbox(time.Now().Add(time.Second)); len(due) != 1 || due[0].Error != "" {
		t.Fatalf("after retry, DueOutbox = %v; want 1 with cleared error", due)
	}
	// Delete removes the row entirely.
	if err := s.DeleteOutbox(row.ID); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.OutboxByID(row.ID); o != nil {
		t.Errorf("after delete, OutboxByID = %+v; want nil", o)
	}
}
