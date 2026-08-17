// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"strings"
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/config"
)

// wrapICS wraps an iCalendar body in a minimal multipart/alternative RFC 822
// message with a text/calendar part, mimicking how Google/Outlook ship invites.
func wrapICS(ics, method string) string {
	ics = strings.ReplaceAll(ics, "\n", "\r\n")
	return "From: Organizer <org@example.com>\r\n" +
		"To: Me <me@example.com>\r\n" +
		"Subject: Invitation\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=b1\r\n\r\n" +
		"--b1\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n\r\n" +
		"You have an invite.\r\n" +
		"--b1\r\n" +
		"Content-Type: text/calendar; charset=UTF-8; method=" + method + "\r\n\r\n" +
		ics + "\r\n" +
		"--b1--\r\n"
}

const googleRequest = `BEGIN:VCALENDAR
PRODID:-//Google Inc//Google Calendar 70.9054//EN
VERSION:2.0
CALSCALE:GREGORIAN
METHOD:REQUEST
BEGIN:VEVENT
DTSTART:20260805T140000Z
DTEND:20260805T150000Z
DTSTAMP:20260723T100000Z
ORGANIZER;CN=Alice Organizer:mailto:alice@example.com
UID:google-uid-123@google.com
ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE;CN=Me:mailto:me@example.com
ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;CN=Alice Organizer:mailto:alice@example.com
SEQUENCE:0
SUMMARY:Project sync
LOCATION:Meeting Room 4
DESCRIPTION:Weekly project sync-up.
RRULE:FREQ=WEEKLY;BYDAY=TU
END:VEVENT
END:VCALENDAR`

const outlookRequest = `BEGIN:VCALENDAR
METHOD:REQUEST
PRODID:Microsoft Exchange Server 2010
VERSION:2.0
BEGIN:VTIMEZONE
TZID:W. Europe Standard Time
BEGIN:STANDARD
DTSTART:16011028T030000
TZOFFSETFROM:+0200
TZOFFSETTO:+0100
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
ORGANIZER;CN=Bob:mailto:bob@contoso.com
ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE;CN=Me:mailto:me@example.com
SUMMARY:Outlook meeting
DTSTART;TZID=W. Europe Standard Time:20260310T093000
DTEND;TZID=W. Europe Standard Time:20260310T103000
UID:outlook-uid-999
SEQUENCE:2
LOCATION:Teams
END:VEVENT
END:VCALENDAR`

const romeRequest = `BEGIN:VCALENDAR
METHOD:REQUEST
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:rome-uid-1
SUMMARY:Pranzo
DTSTART;TZID=Europe/Rome:20260715T130000
DTEND;TZID=Europe/Rome:20260715T140000
DTSTAMP:20260601T100000Z
ORGANIZER:mailto:host@example.it
ATTENDEE;PARTSTAT=NEEDS-ACTION;CN=Me:mailto:me@example.com
SEQUENCE:0
END:VEVENT
END:VCALENDAR`

const allDayRequest = `BEGIN:VCALENDAR
METHOD:REQUEST
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:allday-1
SUMMARY:Company holiday
DTSTART;VALUE=DATE:20260704
DTEND;VALUE=DATE:20260705
DTSTAMP:20260601T100000Z
ORGANIZER:mailto:hr@example.com
END:VEVENT
END:VCALENDAR`

const cancelRequest = `BEGIN:VCALENDAR
METHOD:CANCEL
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:google-uid-123@google.com
SUMMARY:Project sync
DTSTART:20260805T140000Z
DTSTAMP:20260724T100000Z
ORGANIZER:mailto:alice@example.com
ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:me@example.com
SEQUENCE:1
STATUS:CANCELLED
END:VEVENT
END:VCALENDAR`

const replyPayload = `BEGIN:VCALENDAR
METHOD:REPLY
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:google-uid-123@google.com
SUMMARY:Project sync
DTSTAMP:20260723T110000Z
ORGANIZER:mailto:alice@example.com
ATTENDEE;PARTSTAT=ACCEPTED;CN=Me:mailto:me@example.com
SEQUENCE:0
END:VEVENT
END:VCALENDAR`

func TestParseCalendar(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		method   string
		uid      string
		summary  string
		seq      int
		allDay   bool
		nAttend  int
		recur    string
		location string
	}{
		{"google", wrapICS(googleRequest, "REQUEST"), "REQUEST", "google-uid-123@google.com", "Project sync", 0, false, 2, "Weekly on Tuesday", "Meeting Room 4"},
		{"outlook", wrapICS(outlookRequest, "REQUEST"), "REQUEST", "outlook-uid-999", "Outlook meeting", 2, false, 1, "", "Teams"},
		{"rome", wrapICS(romeRequest, "REQUEST"), "REQUEST", "rome-uid-1", "Pranzo", 0, false, 1, "", ""},
		{"allday", wrapICS(allDayRequest, "REQUEST"), "REQUEST", "allday-1", "Company holiday", 0, true, 0, "", ""},
		{"cancel", wrapICS(cancelRequest, "CANCEL"), "CANCEL", "google-uid-123@google.com", "Project sync", 1, false, 1, "", ""},
		{"reply", wrapICS(replyPayload, "REPLY"), "REPLY", "google-uid-123@google.com", "Project sync", 0, false, 1, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := ParseCalendar([]byte(tc.raw))
			if inv == nil {
				t.Fatal("ParseCalendar returned nil")
			}
			if inv.Method != tc.method {
				t.Errorf("method = %q, want %q", inv.Method, tc.method)
			}
			if len(inv.Events) != 1 {
				t.Fatalf("got %d events, want 1", len(inv.Events))
			}
			ev := inv.Events[0]
			if ev.UID != tc.uid {
				t.Errorf("uid = %q, want %q", ev.UID, tc.uid)
			}
			if ev.Summary != tc.summary {
				t.Errorf("summary = %q, want %q", ev.Summary, tc.summary)
			}
			if ev.Sequence != tc.seq {
				t.Errorf("sequence = %d, want %d", ev.Sequence, tc.seq)
			}
			if ev.AllDay != tc.allDay {
				t.Errorf("allDay = %v, want %v", ev.AllDay, tc.allDay)
			}
			if len(ev.Attendees) != tc.nAttend {
				t.Errorf("attendees = %d, want %d", len(ev.Attendees), tc.nAttend)
			}
			if ev.Recurrence != tc.recur {
				t.Errorf("recurrence = %q, want %q", ev.Recurrence, tc.recur)
			}
			if ev.Location != tc.location {
				t.Errorf("location = %q, want %q", ev.Location, tc.location)
			}
		})
	}
}

// TestRomeWallTime asserts a Europe/Rome TZID event yields the correct instant:
// 13:00 Rome (CEST, +02:00 in July) == 11:00 UTC.
func TestRomeWallTime(t *testing.T) {
	inv := ParseCalendar([]byte(wrapICS(romeRequest, "REQUEST")))
	if inv == nil || len(inv.Events) != 1 {
		t.Fatal("parse failed")
	}
	got := inv.Events[0].Start.UTC()
	want := time.Date(2026, 7, 15, 11, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("start = %v, want %v (UTC)", got, want)
	}
	if inv.Events[0].TZID != "Europe/Rome" {
		t.Errorf("tzid = %q, want Europe/Rome", inv.Events[0].TZID)
	}
}

// TestOutlookWindowsZone asserts the Windows zone name resolves so the wall
// time is interpreted at the mapped IANA offset (Europe/Berlin, +01:00 in
// March before DST): 09:30 -> 08:30 UTC.
func TestOutlookWindowsZone(t *testing.T) {
	inv := ParseCalendar([]byte(wrapICS(outlookRequest, "REQUEST")))
	if inv == nil || len(inv.Events) != 1 {
		t.Fatal("parse failed")
	}
	got := inv.Events[0].Start.UTC()
	want := time.Date(2026, 3, 10, 8, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("start = %v, want %v (UTC)", got, want)
	}
}

func TestMatchAttendee(t *testing.T) {
	inv := ParseCalendar([]byte(wrapICS(googleRequest, "REQUEST")))
	att, ok := inv.Events[0].MatchAttendee([]string{"Me <me@example.com>"})
	if !ok {
		t.Fatal("expected to match me@example.com")
	}
	if att.PartStat != "NEEDS-ACTION" {
		t.Errorf("partstat = %q, want NEEDS-ACTION", att.PartStat)
	}
	if _, ok := inv.Events[0].MatchAttendee([]string{"other@nowhere.com"}); ok {
		t.Error("should not match an unrelated address")
	}
}

func TestParseCalendarMalformed(t *testing.T) {
	cases := []string{
		"",
		"not a mime message at all",
		wrapICS("BEGIN:VCALENDAR\nGARBAGE\nno end", "REQUEST"),
		"From: a@b.c\r\nContent-Type: text/plain\r\n\r\njust text, no calendar\r\n",
		wrapICS("BEGIN:VCALENDAR\nVERSION:2.0\nEND:VCALENDAR", "PUBLISH"), // no VEVENT
	}
	for i, raw := range cases {
		// Must not panic; nil is the acceptable "no valid invite" outcome.
		if inv := ParseCalendar([]byte(raw)); inv != nil && len(inv.Events) == 0 {
			t.Errorf("case %d: returned non-nil invite with no events", i)
		}
	}
}

func TestBuildReplyICS(t *testing.T) {
	inv := ParseCalendar([]byte(wrapICS(googleRequest, "REQUEST")))
	ev := inv.Events[0]
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	ics, err := BuildReplyICS(ev, "ACCEPTED", "me@example.com", "Matteo", now)
	if err != nil {
		t.Fatal(err)
	}
	s := string(ics)
	for _, want := range []string{
		"METHOD:REPLY",
		"UID:google-uid-123@google.com",
		"PARTSTAT=ACCEPTED",
		"mailto:me@example.com",
		"ORGANIZER",
		"mailto:alice@example.com",
		"SEQUENCE:0",
		"DTSTAMP:20260723T120000Z",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("reply ics missing %q\n---\n%s", want, s)
		}
	}
	// Round-trips back to a parseable REPLY with our PARTSTAT.
	re := parseICS(ics)
	if re == nil || re.Method != "REPLY" {
		t.Fatalf("reply does not re-parse as REPLY: %+v", re)
	}
	if att, ok := re.Events[0].MatchAttendee([]string{"me@example.com"}); !ok || att.PartStat != "ACCEPTED" {
		t.Errorf("re-parsed reply attendee = %+v (ok=%v)", att, ok)
	}
}

func TestBuildReplyEmail(t *testing.T) {
	inv := ParseCalendar([]byte(wrapICS(googleRequest, "REQUEST")))
	ev := inv.Events[0]
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	ics, _ := BuildReplyICS(ev, "DECLINED", "me@example.com", "Matteo", now)
	raw, msgID, err := buildReplyEmail(config.Account{Name: "acct", Email: "me@example.com", SenderName: "Matteo"},
		ev, "DECLINED", "me@example.com", ev.Organizer.Email, ics, now)
	if err != nil {
		t.Fatal(err)
	}
	if msgID == "" {
		t.Error("empty message id")
	}
	s := string(raw)
	for _, want := range []string{
		"To: <alice@example.com>",
		"Declined: Project sync",
		"multipart/alternative",
		"text/calendar",
		"method=REPLY",
		"has declined: Project sync",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("reply email missing %q", want)
		}
	}
	// The built email must itself re-parse as a REPLY invite.
	if re := ParseCalendar(raw); re == nil || re.Method != "REPLY" {
		t.Errorf("built reply email does not re-parse as REPLY invite")
	}
}

// TestParseBodyCapturesCalendar verifies the invite bytes are captured during
// body parsing and survive the gob cache round-trip, so CalendarInvite can
// serve them without a second IMAP fetch.
func TestParseBodyCapturesCalendar(t *testing.T) {
	raw := []byte(wrapICS(googleRequest, "REQUEST"))
	b := parseBody(raw)
	if len(b.calendar) == 0 {
		t.Fatal("parseBody did not capture the calendar part")
	}
	if inv := parseICS(b.calendar); inv == nil || inv.Method != "REQUEST" {
		t.Fatalf("captured calendar did not parse as REQUEST: %+v", inv)
	}
	blob, err := encodeBody(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeBody(blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.calendar) != string(b.calendar) {
		t.Error("calendar bytes lost across encode/decode")
	}
}
