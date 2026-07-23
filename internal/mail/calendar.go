package mail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-message"
	emmail "github.com/emersion/go-message/mail"

	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/store"
)

// CalPerson is an organizer or attendee reference from an iCalendar object.
type CalPerson struct {
	Name  string // CN param, or the bare address when absent
	Email string // lower-cased bare address (mailto: stripped)
}

// CalAttendee is a CalPerson plus their RSVP state.
type CalAttendee struct {
	CalPerson
	PartStat string // NEEDS-ACTION | ACCEPTED | DECLINED | TENTATIVE | ...
	Role     string // REQ-PARTICIPANT | OPT-PARTICIPANT | CHAIR | ...
}

// CalEvent is one VEVENT flattened into the fields the reading-pane card needs.
type CalEvent struct {
	UID         string
	Sequence    int
	Summary     string
	Description string
	Location    string
	Start       time.Time
	End         time.Time
	AllDay      bool
	TZID        string // original TZID of a timed event ("" for UTC/all-day)
	Organizer   CalPerson
	Attendees   []CalAttendee
	Recurrence  string // human summary, e.g. "Weekly on Tuesday" ("" if none)
	Cancelled   bool   // STATUS:CANCELLED on the event itself
}

// Invite is the parsed calendar payload of a message: its METHOD plus events.
type Invite struct {
	Method string // REQUEST | REPLY | CANCEL | PUBLISH | "" (unknown)
	Events []CalEvent
}

// ParseCalendar scans a raw RFC 822 message for a calendar payload (an inline
// text/calendar part or a .ics attachment) and returns the parsed invite, or
// nil when the message carries no (valid) calendar. It never panics on
// malformed input — a bad payload yields nil, never a broken render.
func ParseCalendar(raw []byte) *Invite {
	blob := extractCalendarPart(raw)
	if blob == nil {
		return nil
	}
	return parseICS(blob)
}

// extractCalendarPart returns the bytes of the first text/calendar part (or
// .ics attachment) in the message, transfer-decoded by go-message.
func extractCalendarPart(raw []byte) []byte {
	ent, err := message.Read(bytes.NewReader(raw))
	if ent == nil {
		if err != nil {
			// Not a MIME message — maybe it *is* a bare .ics file.
			if bytes.Contains(raw, []byte("BEGIN:VCALENDAR")) {
				return raw
			}
		}
		return nil
	}
	var found, fallback []byte
	_ = ent.Walk(func(_ []int, part *message.Entity, perr error) error {
		if found != nil {
			return nil
		}
		if perr != nil && !message.IsUnknownCharset(perr) && !message.IsUnknownEncoding(perr) {
			return nil
		}
		mt, params, _ := part.Header.ContentType()
		mt = strings.ToLower(mt)
		name := strings.ToLower(params["name"])
		disp, dparams, _ := part.Header.ContentDisposition()
		_ = disp
		if fn := strings.ToLower(dparams["filename"]); fn != "" {
			name = fn
		}
		isCal := mt == "text/calendar" || mt == "application/ics"
		isICSFile := strings.HasSuffix(name, ".ics")
		if !isCal && !isICSFile {
			return nil
		}
		data, _ := io.ReadAll(io.LimitReader(part.Body, 5<<20)) // ponytail: 5MB/ics cap
		if !bytes.Contains(data, []byte("BEGIN:VCALENDAR")) {
			return nil
		}
		// Prefer an inline text/calendar (carries METHOD) over a bare .ics attachment.
		if mt == "text/calendar" {
			found = data
		} else if fallback == nil {
			fallback = data
		}
		return nil
	})
	if found != nil {
		return found
	}
	return fallback
}

// parseICS decodes iCalendar bytes into an Invite. Returns nil on decode
// failure or when there are no events.
func parseICS(blob []byte) *Invite {
	cal, err := ical.NewDecoder(bytes.NewReader(blob)).Decode()
	if err != nil || cal == nil {
		return nil
	}
	inv := &Invite{Method: strings.ToUpper(propText(cal.Props, ical.PropMethod))}
	for _, ev := range cal.Events() {
		inv.Events = append(inv.Events, parseEvent(&ev))
	}
	if len(inv.Events) == 0 {
		return nil
	}
	return inv
}

func parseEvent(ev *ical.Event) CalEvent {
	out := CalEvent{
		UID:         propText(ev.Props, ical.PropUID),
		Summary:     propText(ev.Props, ical.PropSummary),
		Description: propText(ev.Props, ical.PropDescription),
		Location:    propText(ev.Props, ical.PropLocation),
	}
	if seq := ev.Props.Get(ical.PropSequence); seq != nil {
		out.Sequence, _ = strconv.Atoi(strings.TrimSpace(seq.Value))
	}
	if st := ev.Props.Get(ical.PropStatus); st != nil && strings.EqualFold(st.Value, "CANCELLED") {
		out.Cancelled = true
	}
	out.Start, out.AllDay, out.TZID = icalTime(ev.Props.Get(ical.PropDateTimeStart))
	out.End, _, _ = icalTime(ev.Props.Get(ical.PropDateTimeEnd))
	out.Organizer = parsePerson(ev.Props.Get(ical.PropOrganizer))
	for i := range ev.Props[ical.PropAttendee] {
		p := &ev.Props[ical.PropAttendee][i]
		a := CalAttendee{
			CalPerson: parsePerson(p),
			PartStat:  strings.ToUpper(p.Params.Get(ical.ParamParticipationStatus)),
			Role:      strings.ToUpper(p.Params.Get(ical.ParamRole)),
		}
		if a.PartStat == "" {
			a.PartStat = "NEEDS-ACTION"
		}
		out.Attendees = append(out.Attendees, a)
	}
	out.Recurrence = recurrenceSummary(ev.Props.Get(ical.PropRecurrenceRule))
	return out
}

// MatchAttendee finds the attendee whose address is one of the receiving
// account's identities (primary + aliases), returning it and true. Used to show
// the user's own current RSVP state on the card.
func (e CalEvent) MatchAttendee(myEmails []string) (CalAttendee, bool) {
	set := map[string]bool{}
	for _, m := range myEmails {
		if m = strings.ToLower(strings.TrimSpace(bareAddr(m))); m != "" {
			set[m] = true
		}
	}
	for _, a := range e.Attendees {
		if set[a.Email] {
			return a, true
		}
	}
	return CalAttendee{}, false
}

// --- helpers ---

func propText(props ical.Props, name string) string {
	if p := props.Get(name); p != nil {
		return strings.TrimSpace(p.Value)
	}
	return ""
}

func parsePerson(p *ical.Prop) CalPerson {
	if p == nil {
		return CalPerson{}
	}
	email := strings.TrimSpace(p.Value)
	if i := strings.Index(strings.ToLower(email), "mailto:"); i >= 0 {
		email = email[i+len("mailto:"):]
	}
	email = strings.ToLower(strings.TrimSpace(email))
	name := strings.TrimSpace(p.Params.Get(ical.ParamCommonName))
	if name == "" {
		name = email
	}
	return CalPerson{Name: name, Email: email}
}

// icalTime parses a DTSTART/DTEND property robustly: all-day (VALUE=DATE),
// UTC (trailing Z), or a TZID-anchored wall time. A TZID that time.LoadLocation
// can't resolve (e.g. an Outlook Windows zone we don't map) falls back to local
// time but keeps the original TZID string for display, so the card degrades
// gracefully instead of erroring.
func icalTime(p *ical.Prop) (t time.Time, allDay bool, tzid string) {
	if p == nil {
		return time.Time{}, false, ""
	}
	v := strings.TrimSpace(p.Value)
	if len(v) == 8 && !strings.ContainsAny(v, "T") { // 20060102
		t, _ = time.ParseInLocation("20060102", v, time.Local)
		return t, true, ""
	}
	if strings.HasSuffix(v, "Z") {
		t, _ = time.ParseInLocation("20060102T150405Z", v, time.UTC)
		return t, false, ""
	}
	tzid = strings.TrimSpace(p.Params.Get(ical.ParamTimezoneID))
	loc := time.Local
	if tzid != "" {
		if l, err := time.LoadLocation(tzid); err == nil {
			loc = l
		} else if l := windowsZone(tzid); l != nil {
			loc = l
		}
	}
	t, _ = time.ParseInLocation("20060102T150405", v, loc)
	return t, false, tzid
}

// windowsZone maps the handful of common Windows/Exchange zone names to IANA
// locations. ponytail: not the full CLDR table — the long tail falls back to
// local time (icalTime), extend this map if a real invite lands in the wrong
// wall time.
func windowsZone(name string) *time.Location {
	m := map[string]string{
		"W. Europe Standard Time":      "Europe/Berlin",
		"Central Europe Standard Time": "Europe/Budapest",
		"Romance Standard Time":        "Europe/Paris",
		"GMT Standard Time":            "Europe/London",
		"Eastern Standard Time":        "America/New_York",
		"Central Standard Time":        "America/Chicago",
		"Pacific Standard Time":        "America/Los_Angeles",
		"UTC":                          "UTC",
	}
	if iana, ok := m[name]; ok {
		if l, err := time.LoadLocation(iana); err == nil {
			return l
		}
	}
	return nil
}

// recurrenceSummary renders a short human description of an RRULE for the common
// FREQ/INTERVAL/BYDAY cases, falling back to the raw rule text.
func recurrenceSummary(p *ical.Prop) string {
	if p == nil {
		return ""
	}
	parts := map[string]string{}
	for _, kv := range strings.Split(p.Value, ";") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			parts[strings.ToUpper(strings.TrimSpace(k))] = strings.ToUpper(strings.TrimSpace(v))
		}
	}
	freq := parts["FREQ"]
	if freq == "" {
		return ""
	}
	interval := 1
	if n, err := strconv.Atoi(parts["INTERVAL"]); err == nil && n > 0 {
		interval = n
	}
	unit := map[string]string{"DAILY": "day", "WEEKLY": "week", "MONTHLY": "month", "YEARLY": "year"}[freq]
	if unit == "" {
		return strings.TrimSpace(p.Value) // uncommon FREQ (HOURLY/…): raw fallback
	}
	var b strings.Builder
	switch {
	case interval == 1 && freq == "DAILY":
		b.WriteString("Daily")
	case interval == 1 && freq == "WEEKLY":
		b.WriteString("Weekly")
	case interval == 1 && freq == "MONTHLY":
		b.WriteString("Monthly")
	case interval == 1 && freq == "YEARLY":
		b.WriteString("Yearly")
	case interval == 1:
		fmt.Fprintf(&b, "Every %s", unit)
	default:
		fmt.Fprintf(&b, "Every %d %ss", interval, unit)
	}
	if days := parts["BYDAY"]; days != "" && freq == "WEEKLY" {
		if named := weekdayNames(days); named != "" {
			fmt.Fprintf(&b, " on %s", named)
		}
	}
	if cnt := parts["COUNT"]; cnt != "" {
		fmt.Fprintf(&b, ", %s times", cnt)
	} else if until := parts["UNTIL"]; until != "" {
		if t, _, _ := icalTime(&ical.Prop{Name: ical.PropDateTimeStart, Value: until, Params: ical.Params{}}); !t.IsZero() {
			fmt.Fprintf(&b, ", until %s", t.Format("2 Jan 2006"))
		}
	}
	return b.String()
}

func weekdayNames(byday string) string {
	names := map[string]string{
		"MO": "Monday", "TU": "Tuesday", "WE": "Wednesday", "TH": "Thursday",
		"FR": "Friday", "SA": "Saturday", "SU": "Sunday",
	}
	var out []string
	for _, d := range strings.Split(byday, ",") {
		d = strings.TrimSpace(d)
		// strip an ordinal prefix like "2TU"
		d = strings.TrimLeft(d, "+-0123456789")
		if n, ok := names[d]; ok {
			out = append(out, n)
		}
	}
	return strings.Join(out, ", ")
}

// --- iTIP REPLY construction + send ---

// partStatVerb maps a PARTSTAT to its human past-tense phrasing.
func partStatVerb(partstat string) string {
	switch strings.ToUpper(partstat) {
	case "ACCEPTED":
		return "accepted"
	case "DECLINED":
		return "declined"
	case "TENTATIVE":
		return "tentatively accepted"
	default:
		return "responded to"
	}
}

// BuildReplyICS builds a METHOD:REPLY VCALENDAR carrying the user's single
// attendee entry with the chosen PARTSTAT, echoing the request's UID, SEQUENCE
// and organizer (RFC 5546). Returns the encoded ics bytes.
func BuildReplyICS(ev CalEvent, partstat, myEmail, myName string, now time.Time) ([]byte, error) {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//SM//Calendar//EN")
	cal.Props.SetText(ical.PropMethod, "REPLY")

	e := ical.NewEvent()
	e.Props.SetText(ical.PropUID, ev.UID)
	if ev.Summary != "" {
		e.Props.SetText(ical.PropSummary, ev.Summary)
	}
	e.Props.SetDateTime(ical.PropDateTimeStamp, now.UTC())
	seq := ical.NewProp(ical.PropSequence)
	seq.Value = strconv.Itoa(ev.Sequence)
	e.Props.Set(seq)

	if ev.Organizer.Email != "" {
		org := ical.NewProp(ical.PropOrganizer)
		if ev.Organizer.Name != "" && ev.Organizer.Name != ev.Organizer.Email {
			org.Params.Set(ical.ParamCommonName, ev.Organizer.Name)
		}
		org.Value = "mailto:" + ev.Organizer.Email
		e.Props.Set(org)
	}

	att := ical.NewProp(ical.PropAttendee)
	if myName != "" && myName != myEmail {
		att.Params.Set(ical.ParamCommonName, myName)
	}
	att.Params.Set(ical.ParamParticipationStatus, strings.ToUpper(partstat))
	att.Value = "mailto:" + myEmail
	e.Props.Set(att)

	cal.Children = append(cal.Children, e.Component)
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildReplyEmail assembles the RFC 822 iTIP reply message: a
// multipart/alternative with a short text/plain note and the REPLY
// text/calendar part (method=REPLY).
func buildReplyEmail(cfg config.Account, ev CalEvent, partstat, fromAddr, toAddr string, ics []byte, now time.Time) ([]byte, string, error) {
	name := cfg.DisplayNameFor(fromAddr)
	var h emmail.Header
	h.SetAddressList("From", []*emmail.Address{{Name: name, Address: fromAddr}})
	h.SetAddressList("To", []*emmail.Address{{Address: toAddr}})
	title := ev.Summary
	if title == "" {
		title = "(no title)"
	}
	verb := partStatVerb(partstat)
	h.SetSubject(capitalize(verb) + ": " + title)
	h.SetDate(now)
	if err := h.GenerateMessageIDWithHostname(msgIDHost(fromAddr)); err != nil {
		return nil, "", err
	}
	msgID, _ := h.MessageID()

	var buf bytes.Buffer
	iw, err := emmail.CreateInlineWriter(&buf, h) // multipart/alternative
	if err != nil {
		return nil, "", err
	}
	var tp emmail.InlineHeader
	tp.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	tw, err := iw.CreatePart(tp)
	if err != nil {
		return nil, "", err
	}
	note := fmt.Sprintf("%s has %s: %s", name, verb, title)
	if _, err := io.WriteString(tw, toCRLF(note)); err != nil {
		return nil, "", err
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	var cp emmail.InlineHeader
	cp.SetContentType("text/calendar", map[string]string{"charset": "utf-8", "method": "REPLY", "component": "VEVENT"})
	cw, err := iw.CreatePart(cp)
	if err != nil {
		return nil, "", err
	}
	if _, err := cw.Write(ics); err != nil {
		return nil, "", err
	}
	if err := cw.Close(); err != nil {
		return nil, "", err
	}
	if err := iw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), msgID, nil
}

// SendCalendarReply builds and sends an iTIP REPLY for event ev as partstat
// from account accountName, using fromAddr as the sending identity (an account
// alias or the primary address). The reply is addressed to the event's
// organizer. Mirrors Manager.Send's delivery + Sent-append behaviour.
func (m *Manager) SendCalendarReply(ctx context.Context, accountName, fromAddr string, ev CalEvent, partstat string) error {
	a := m.accounts[accountName]
	if a == nil {
		return fmt.Errorf("unknown account %q", accountName)
	}
	if ev.UID == "" {
		return fmt.Errorf("event has no UID")
	}
	if ev.Organizer.Email == "" {
		return fmt.Errorf("event has no organizer to reply to")
	}
	if fromAddr == "" {
		fromAddr = a.cfg.Email
	}
	fromAddr = bareAddr(fromAddr)
	now := time.Now()
	ics, err := BuildReplyICS(ev, partstat, fromAddr, a.cfg.DisplayNameFor(fromAddr), now)
	if err != nil {
		return fmt.Errorf("build reply ics: %w", err)
	}
	raw, _, err := buildReplyEmail(a.cfg, ev, partstat, fromAddr, ev.Organizer.Email, ics, now)
	if err != nil {
		return fmt.Errorf("build reply email: %w", err)
	}
	var token string
	if a.cfg.Auth == "oauth2" {
		if token, err = m.accessToken(ctx, accountName); err != nil {
			return err
		}
	}
	if err := smtpSend(a.cfg, token, fromAddr, []string{ev.Organizer.Email}, raw); err != nil {
		return err
	}
	if a.cfg.Provider != "gmail" {
		if err := m.appendSent(ctx, a, raw); err != nil {
			// Delivered already — a missing Sent copy is not fatal.
			_ = err
		}
	}
	return nil
}

// CalendarInvite returns the message's parsed calendar payload, or nil when it
// carries no invite. It reuses the cached parsed body (LRU → SQLite → IMAP), so
// opening an invite costs no IMAP fetch beyond the one the body already does.
func (m *Manager) CalendarInvite(ctx context.Context, msg *store.Message) (*Invite, error) {
	body, err := m.parsedBody(ctx, msg, false)
	if err != nil {
		return nil, err
	}
	if len(body.calendar) == 0 {
		return nil, nil
	}
	return parseICS(body.calendar), nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
