// Package filter is the pure rules engine for auto-filtering incoming mail:
// conditions (from/to/subject/body, contains/regex) AND together, and a list
// of actions to run when they all match. No I/O here — the sync loop and the
// HTTP layer (http.go) adapt this to their own message/storage types.
package filter

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Condition fields.
const (
	FieldFrom    = "from"
	FieldTo      = "to"
	FieldSubject = "subject"
	FieldBody    = "body"
)

// Condition operators.
const (
	OpContains = "contains"
	OpRegex    = "regex"
)

// Action types.
const (
	ActionForward  = "forward"
	ActionMove     = "move"
	ActionMarkRead = "mark_read"
	ActionStar     = "star"
	ActionDelete   = "delete"
	ActionLabel    = "label"
	// ActionNotify sends a notification (Web Push and/or ntfy) for the matched
	// message. It takes no argument: what a notification says and where it goes
	// is configured once in Settings → Notifications, not per rule.
	ActionNotify = "notify"
)

// MessageMeta is the subset of a message a rule can match against. The sync
// engine adapts its own message representation into this.
type MessageMeta struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

type Condition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

type Action struct {
	Type string `json:"type"`
	Arg  string `json:"arg,omitempty"`
}

// Rule is a named, ordered, per-account (or global, Account == "") filter.
type Rule struct {
	ID         int64       `json:"id"`
	Account    string      `json:"account"`
	Name       string      `json:"name"`
	Position   int         `json:"position"`
	Enabled    bool        `json:"enabled"`
	Conditions []Condition `json:"conditions"`
	Actions    []Action    `json:"actions"`
}

// fieldValue picks the message field a condition targets.
func (c Condition) fieldValue(m MessageMeta) (string, error) {
	switch strings.ToLower(c.Field) {
	case FieldFrom:
		return m.From, nil
	case FieldTo:
		return m.To, nil
	case FieldSubject:
		return m.Subject, nil
	case FieldBody:
		return m.Body, nil
	default:
		return "", fmt.Errorf("filter: unknown field %q", c.Field)
	}
}

// matches evaluates a single condition. Regex is compiled on every call:
// rules are evaluated per incoming message (low volume), not in a hot loop.
// ponytail: add a compiled-regex cache if sync throughput ever makes this show up in profiles.
func (c Condition) matches(m MessageMeta) (bool, error) {
	v, err := c.fieldValue(m)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(c.Op) {
	case OpContains:
		return strings.Contains(strings.ToLower(v), strings.ToLower(c.Value)), nil
	case OpRegex:
		re, err := regexp.Compile(c.Value)
		if err != nil {
			return false, fmt.Errorf("filter: invalid regex %q: %w", c.Value, err)
		}
		return re.MatchString(v), nil
	default:
		return false, fmt.Errorf("filter: unknown op %q", c.Op)
	}
}

// Matches reports whether every condition on the rule matches m (AND).
// A rule with no conditions never matches (avoids an accidental catch-all)
// and an invalid condition (bad field/op/regex) is treated as a non-match
// rather than a panic — validate rules at write time via Validate.
func (r Rule) Matches(m MessageMeta) bool {
	if len(r.Conditions) == 0 {
		return false
	}
	for _, c := range r.Conditions {
		ok, err := c.matches(m)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// DryRun returns the subset of msgs that r would match.
func DryRun(r Rule, msgs []MessageMeta) []MessageMeta {
	var out []MessageMeta
	for _, m := range msgs {
		if r.Matches(m) {
			out = append(out, m)
		}
	}
	return out
}

var validActionTypes = map[string]bool{
	ActionForward: true, ActionMove: true, ActionMarkRead: true,
	ActionStar: true, ActionDelete: true, ActionLabel: true,
	ActionNotify: true,
}

// Validate checks a rule is well-formed before it's stored: known
// field/op/action names, non-empty values, and that any regex actually
// compiles. Never let a bad rule reach the database silently.
func (r Rule) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("name is required")
	}
	if len(r.Conditions) == 0 {
		return errors.New("at least one condition is required")
	}
	for _, c := range r.Conditions {
		if _, err := c.fieldValue(MessageMeta{}); err != nil {
			return err
		}
		if strings.TrimSpace(c.Value) == "" {
			return fmt.Errorf("condition on %q needs a value", c.Field)
		}
		switch strings.ToLower(c.Op) {
		case OpContains:
		case OpRegex:
			if _, err := regexp.Compile(c.Value); err != nil {
				return fmt.Errorf("invalid regex %q: %w", c.Value, err)
			}
		default:
			return fmt.Errorf("unknown operator %q", c.Op)
		}
	}
	if len(r.Actions) == 0 {
		return errors.New("at least one action is required")
	}
	for _, a := range r.Actions {
		if !validActionTypes[a.Type] {
			return fmt.Errorf("unknown action %q", a.Type)
		}
		if (a.Type == ActionForward || a.Type == ActionMove || a.Type == ActionLabel) && strings.TrimSpace(a.Arg) == "" {
			return fmt.Errorf("action %q needs an argument", a.Type)
		}
	}
	return nil
}
