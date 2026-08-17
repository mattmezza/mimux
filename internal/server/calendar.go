// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"strings"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/mail"
)

// attendeeView is one row in the card's expandable attendee list.
type attendeeView struct {
	Name   string
	Email  string
	Status string // human PARTSTAT: "Accepted" | "Declined" | "Tentative" | "No response"
}

// inviteView is the fully-formatted event card model — all date/time/timezone
// formatting is done server-side so the template stays declarative.
type inviteView struct {
	MsgID   int64
	CSRF    string
	HasCard bool

	DateMonth   string // "AUG"
	DateDay     string // "5"
	Title       string
	When        string // "Wed, 5 Aug 2026"
	TimeRange   string // "16:00 – 17:00 CEST" or "All day"
	OrigTZ      string // original TZID when it differs from the local zone, else ""
	Location    string
	Description string

	OrganizerName  string
	OrganizerEmail string
	Attendees      []attendeeView
	AttendeeCount  int

	Recurrence string
	MoreCount  int // extra VEVENTs beyond the first ("+N more")

	StatusStrip string // "This event was cancelled" | "Updated invitation" | ""
	Cancelled   bool
	MyStatus    string // "accepted" | "tentative" | "declined" | "" (for button highlight)
	ShowButtons bool
}

// accountEmails returns an account's identity addresses: its primary plus every
// configured alias. Used to match the user's own attendee entry on an invite.
func (s *Server) accountEmails(name string) []string {
	a, ok := s.accountByName(name)
	if !ok {
		return nil
	}
	out := []string{a.Email}
	for _, al := range a.Aliases {
		if al.Email != "" {
			out = append(out, al.Email)
		}
	}
	return out
}

func partStatHuman(ps string) string {
	switch strings.ToUpper(ps) {
	case "ACCEPTED":
		return "Accepted"
	case "DECLINED":
		return "Declined"
	case "TENTATIVE":
		return "Tentative"
	default:
		return "No response"
	}
}

// buildInviteView turns a parsed event into the card model, applying the local
// account identities (for the user's own attendee entry) and any recorded RSVP.
func (s *Server) buildInviteView(msgID int64, csrf, account, method string, inv *mail.Invite) inviteView {
	v := inviteView{MsgID: msgID, CSRF: csrf}
	if inv == nil || len(inv.Events) == 0 {
		return v
	}
	ev := inv.Events[0]
	v.HasCard = true
	v.MoreCount = len(inv.Events) - 1
	v.Title = ev.Summary
	if v.Title == "" {
		v.Title = "(no title)"
	}
	v.Location = ev.Location
	v.Description = ev.Description
	v.Recurrence = ev.Recurrence
	v.OrganizerName = ev.Organizer.Name
	v.OrganizerEmail = ev.Organizer.Email

	// Date/time: render in server-local time, noting the original zone when it
	// differs from local.
	start := ev.Start
	if !start.IsZero() {
		local := start.Local()
		v.DateMonth = strings.ToUpper(local.Format("Jan"))
		v.DateDay = local.Format("2")
		v.When = local.Format("Mon, 2 Jan 2006")
		if ev.AllDay {
			v.TimeRange = "All day"
		} else {
			end := ev.End
			tr := local.Format("15:04")
			if !end.IsZero() {
				tr += " – " + end.Local().Format("15:04")
			}
			tr += " " + local.Format("MST")
			v.TimeRange = tr
			if ev.TZID != "" && ev.TZID != local.Location().String() {
				v.OrigTZ = ev.TZID
			}
		}
	}

	for _, a := range ev.Attendees {
		v.Attendees = append(v.Attendees, attendeeView{Name: a.Name, Email: a.Email, Status: partStatHuman(a.PartStat)})
	}
	v.AttendeeCount = len(v.Attendees)

	// The user's own attendee entry + any recorded response.
	_, matched := ev.MatchAttendee(s.accountEmails(account))
	rsvp, hasRSVP, _ := s.store.GetRSVP(account, ev.UID)

	v.Cancelled = method == "CANCEL" || ev.Cancelled
	switch {
	case v.Cancelled:
		v.StatusStrip = "This event was cancelled"
	case ev.Sequence > 0 && (!hasRSVP || rsvp.Sequence < ev.Sequence):
		v.StatusStrip = "Updated invitation"
	}

	// Show the recorded response only when it applies to this (or a newer)
	// version of the event; an older response is stale after an update.
	if hasRSVP && rsvp.Sequence >= ev.Sequence {
		v.MyStatus = strings.ToLower(rsvp.PartStat)
	}
	v.ShowButtons = method == "REQUEST" && matched && ev.Organizer.Email != "" && !v.Cancelled
	return v
}

// handleInvite lazy-loads the calendar-invite card (hx-get on open), mirroring
// the attachments strip. Renders nothing when the message carries no invite.
func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	inv, err := s.mail.CalendarInvite(r.Context(), msg)
	if err != nil || inv == nil {
		s.renderPartial(w, "calendar_invite", inviteView{})
		return
	}
	csrf := auth.EnsureCSRF(w, r, s.secure)
	s.renderPartial(w, "calendar_invite", s.buildInviteView(msg.ID, csrf, msg.Account, inv.Method, inv))
}

// handleRSVP builds + sends an iTIP REPLY for the message's invite with the
// chosen PARTSTAT, records it locally only on a successful send, then re-renders
// the card. A send failure surfaces a toast and leaves the response unrecorded.
func (s *Server) handleRSVP(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	partstat := map[string]string{
		"accept":    "ACCEPTED",
		"tentative": "TENTATIVE",
		"decline":   "DECLINED",
	}[strings.ToLower(r.FormValue("response"))]
	if partstat == "" {
		http.Error(w, "invalid response", http.StatusBadRequest)
		return
	}
	inv, err := s.mail.CalendarInvite(r.Context(), msg)
	if err != nil || inv == nil || len(inv.Events) == 0 {
		http.Error(w, "no invite on this message", http.StatusBadRequest)
		return
	}
	ev := inv.Events[0]
	fromAddr := ""
	if att, ok := ev.MatchAttendee(s.accountEmails(msg.Account)); ok {
		fromAddr = att.Email
	}

	csrf := auth.EnsureCSRF(w, r, s.secure)
	if err := s.mail.SendCalendarReply(r.Context(), msg.Account, fromAddr, ev, partstat); err != nil {
		s.mail.Toast("Could not send response: " + err.Error())
		// Re-render unchanged (do NOT record) so the user can retry.
		s.renderPartial(w, "calendar_invite", s.buildInviteView(msg.ID, csrf, msg.Account, inv.Method, inv))
		return
	}
	if err := s.store.SaveRSVP(msg.Account, ev.UID, partstat, ev.Sequence); err != nil {
		s.mail.Toast("Response sent but not saved locally.")
	}
	v := s.buildInviteView(msg.ID, csrf, msg.Account, inv.Method, inv)
	v.MyStatus = map[string]string{"ACCEPTED": "accepted", "TENTATIVE": "tentative", "DECLINED": "declined"}[partstat]
	v.StatusStrip = "" // clear the "updated invitation" nudge now that we've responded
	s.renderPartial(w, "calendar_invite", v)
}
