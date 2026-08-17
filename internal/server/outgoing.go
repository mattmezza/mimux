package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/store"
)

// soonWindow: queued rows sending within this window render in the "Sending
// soon" group with a live client-side countdown, rather than under "Scheduled".
const soonWindow = 60 * time.Second

// outgoingData is the grouped Outbox view for the /outgoing page and its
// pollable list partial.
type outgoingData struct {
	Soon      []store.Outbox    // queued, due within soonWindow — live countdown
	Scheduled []store.Outbox    // queued, further out
	Failed    []store.Outbox    // status = failed
	Sent      []store.Outbox    // status = sent, last 24h (quiet reassurance)
	Colors    map[string]string // account name -> dot color
	CSRF      string
}

// outgoing assembles the grouped view. Cheap enough to run per request /
// per 10s poll; the row counts here are tiny.
func (s *Server) outgoing(csrf string) outgoingData {
	prefs := s.store.GetPrefs()
	d := outgoingData{Colors: prefs.AccountColors, CSRF: csrf}
	queued, err := s.store.ListScheduled()
	if err != nil {
		slog.Error("outgoing: scheduled", "err", err)
	}
	if d.Failed, err = s.store.ListFailed(); err != nil {
		slog.Error("outgoing: failed", "err", err)
	}
	if d.Sent, err = s.store.ListRecentSent(time.Now().Add(-24 * time.Hour)); err != nil {
		slog.Error("outgoing: sent", "err", err)
	}
	cutoff := time.Now().Add(soonWindow)
	for _, o := range queued {
		if o.SendAt.Before(cutoff) {
			d.Soon = append(d.Soon, o)
		} else {
			d.Scheduled = append(d.Scheduled, o)
		}
	}
	return d
}

func (s *Server) handleOutgoingPage(w http.ResponseWriter, r *http.Request) {
	csrf := auth.EnsureCSRF(w, r, s.secure)
	s.render(w, "outgoing", map[string]any{
		"CSRF":          csrf,
		"Sidebar":       s.sidebarData(),
		"AccountColors": s.store.GetPrefs().AccountColors,
		"Outgoing":      s.outgoing(csrf),
	})
}

// handleOutgoingList renders just the grouped list — polled every ~10s so the
// page stays truthful as rows fire and move between groups.
func (s *Server) handleOutgoingList(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "outgoing_list", s.outgoing(auth.EnsureCSRF(w, r, s.secure)))
}

// handleOutboxCancel cancels a queued message (Cancel button). Handles the race
// where the row already fired: CancelOutbox reports false and we surface a
// friendly toast instead of erroring. Always re-renders the truthful list.
func (s *Server) handleOutboxCancel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ok, err := s.store.CancelOutbox(id)
	if err != nil {
		slog.Error("outbox: cancel", "err", err)
		http.Error(w, "cancel failed", http.StatusInternalServerError)
		return
	}
	if ok {
		s.mail.Toast("Send cancelled.")
	} else {
		s.mail.Toast("Already sent — too late to cancel.")
	}
	s.renderPartial(w, "outgoing_list", s.outgoing(auth.EnsureCSRF(w, r, s.secure)))
}

// handleOutboxRetry re-queues a failed row for immediate send.
func (s *Server) handleOutboxRetry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if ok, err := s.store.RetryOutbox(id); err != nil {
		slog.Error("outbox: retry", "err", err)
	} else if ok {
		s.mail.Toast("Re-queued — sending now.")
	}
	s.renderPartial(w, "outgoing_list", s.outgoing(auth.EnsureCSRF(w, r, s.secure)))
}

// handleOutboxDelete removes an outbox row (Failed-section Delete).
func (s *Server) handleOutboxDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteOutbox(id); err != nil {
		slog.Error("outbox: delete", "err", err)
	}
	s.mail.Toast("Deleted.")
	s.renderPartial(w, "outgoing_list", s.outgoing(auth.EnsureCSRF(w, r, s.secure)))
}
