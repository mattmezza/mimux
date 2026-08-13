package store

import (
	"fmt"
	"sort"
	"time"

	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/filter"
)

// ExportVersion is the envelope version for portable config dumps. Bump when the
// shape changes so future imports can migrate older files. v2 added filters,
// signatures, templates, saved searches, trusted senders and message labels; a
// v1 file still imports (the new sections are simply absent).
const ExportVersion = 2

// TokenExport is an OAuth2 token in the portable dump (RFC3339 expiry string).
type TokenExport struct {
	Account string `json:"account"`
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	Expiry  string `json:"expiry,omitempty"`
}

// SignatureExport is a signature plus the identity addresses linked to it. The
// links travel by address instead of by signature id, which means nothing on
// the instance the dump is restored into.
type SignatureExport struct {
	Signature
	Identities []string `json:"identities,omitempty"`
}

// LabelExport carries the user labels on one message. Labels are the only part
// of the messages table that a re-sync cannot bring back: go-imap can't push
// X-GM-LABELS, so a locally added label exists nowhere but this DB (see
// mail.SetLabel). Keyed on (account, Message-ID) because uids don't survive a
// uidvalidity bump, and because Gmail's INBOX/All Mail copies share the header.
type LabelExport struct {
	Account   string `json:"account"`
	MessageID string `json:"message_id"`
	Labels    string `json:"labels"`
}

// ConfigExport is the full portable config: accounts (incl credentials, aliases
// and per-account sync overrides), every app_settings row (prefs +
// translate/AI keys + colors), OAuth tokens, filter rules, signatures with
// their identity links, templates, saved searches, trusted senders, and the
// local-only message labels.
//
// It excludes sessions and the login user, all mail data (folders, messages,
// bodies, drafts, outbox, calendar RSVPs) and the derived caches (translations,
// summaries, search index/cache/history) — everything there either re-syncs
// from IMAP or is recomputed on demand.
//
// SECURITY: this contains account passwords, OAuth tokens and API keys in
// cleartext.
type ConfigExport struct {
	Version        int               `json:"version"`
	Accounts       []config.Account  `json:"accounts"`
	Settings       map[string]string `json:"settings"`
	Tokens         []TokenExport     `json:"tokens,omitempty"`
	Filters        []filter.Rule     `json:"filters,omitempty"`
	Signatures     []SignatureExport `json:"signatures,omitempty"`
	Templates      []Template        `json:"templates,omitempty"`
	SavedSearches  []SavedSearch     `json:"saved_searches,omitempty"`
	TrustedSenders []string          `json:"trusted_senders,omitempty"`
	Labels         []LabelExport     `json:"labels,omitempty"`
}

// Export builds a ConfigExport snapshot of the current instance.
func (s *Store) Export() (ConfigExport, error) {
	accts, err := s.ListAccounts()
	if err != nil {
		return ConfigExport{}, err
	}
	settings := map[string]string{}
	rows, err := s.DB.Query(`SELECT key, value FROM app_settings`)
	if err != nil {
		return ConfigExport{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return ConfigExport{}, err
		}
		settings[k] = v
	}
	if err := rows.Err(); err != nil {
		return ConfigExport{}, err
	}
	trows, err := s.DB.Query(`SELECT account, access_token, refresh_token, expiry FROM oauth_tokens`)
	if err != nil {
		return ConfigExport{}, err
	}
	defer func() { _ = trows.Close() }()
	var tokens []TokenExport
	for trows.Next() {
		var t TokenExport
		if err := trows.Scan(&t.Account, &t.Access, &t.Refresh, &t.Expiry); err != nil {
			return ConfigExport{}, err
		}
		tokens = append(tokens, t)
	}
	if err := trows.Err(); err != nil {
		return ConfigExport{}, err
	}
	rules, err := s.ListRules()
	if err != nil {
		return ConfigExport{}, err
	}
	sigs, err := s.exportSignatures()
	if err != nil {
		return ConfigExport{}, err
	}
	tpls, err := s.ListTemplates()
	if err != nil {
		return ConfigExport{}, err
	}
	saved, err := s.ListSavedSearches()
	if err != nil {
		return ConfigExport{}, err
	}
	senders, err := s.exportTrustedSenders()
	if err != nil {
		return ConfigExport{}, err
	}
	labels, err := s.exportLabels()
	if err != nil {
		return ConfigExport{}, err
	}
	return ConfigExport{
		Version: ExportVersion, Accounts: accts, Settings: settings, Tokens: tokens,
		Filters: rules, Signatures: sigs, Templates: tpls, SavedSearches: saved,
		TrustedSenders: senders, Labels: labels,
	}, nil
}

// exportSignatures pairs every signature with the identity addresses pointing
// at it (the link table keyed by id, flipped around for the dump).
func (s *Store) exportSignatures() ([]SignatureExport, error) {
	sigs, err := s.ListSignatures()
	if err != nil {
		return nil, err
	}
	links, err := s.IdentitySignatureMap()
	if err != nil {
		return nil, err
	}
	byID := map[int64][]string{}
	for addr, id := range links {
		byID[id] = append(byID[id], addr)
	}
	var out []SignatureExport
	for _, sig := range sigs {
		sort.Strings(byID[sig.ID]) // map order isn't stable; keep dumps diffable
		out = append(out, SignatureExport{Signature: sig, Identities: byID[sig.ID]})
	}
	return out, nil
}

// exportTrustedSenders lists the addresses the user allowed to load external
// content. Only the allowed ones: a "not trusted" row is the default anyway.
func (s *Store) exportTrustedSenders() ([]string, error) {
	rows, err := s.DB.Query(`SELECT address FROM sender_prefs WHERE allow_external = 1 ORDER BY address`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, err
		}
		out = append(out, addr)
	}
	return out, rows.Err()
}

// exportLabels dumps just the label column of labelled messages — no bodies,
// snippets or headers beyond the Message-ID needed to find the row again.
// DISTINCT because Gmail stores the same message once per folder copy.
func (s *Store) exportLabels() ([]LabelExport, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT account, message_id, labels FROM messages
		WHERE labels != '' AND message_id != '' ORDER BY account, message_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LabelExport
	for rows.Next() {
		var l LabelExport
		if err := rows.Scan(&l.Account, &l.MessageID, &l.Labels); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ImportSummary reports what an Import touched.
type ImportSummary struct {
	Accounts   int
	Settings   int
	Tokens     int
	Filters    int
	Signatures int
	Templates  int
	Searches   int
	Senders    int
	Labels     int
}

// Import upserts everything in a ConfigExport. Idempotent: importing a dump taken
// from this same instance changes nothing. Rows that carry an id in the dump
// (filters, signatures, templates) are merged by name, not by id — ids are
// meaningless on another instance, so matching on them would duplicate or
// collide. Older dumps import fine; their missing sections are simply no-ops.
//
// Labels only re-attach to messages that are already synced, so on a rebuilt
// instance import once now for the config and again after the first full sync
// to pick the labels back up.
func (s *Store) Import(e ConfigExport) (ImportSummary, error) {
	var sum ImportSummary
	if e.Version < 1 || e.Version > ExportVersion {
		return sum, fmt.Errorf("import: unsupported version %d (expected 1..%d)", e.Version, ExportVersion)
	}
	for _, a := range e.Accounts {
		if err := config.NormalizeAccount(&a); err != nil {
			return sum, fmt.Errorf("import: %w", err)
		}
		if err := s.UpsertAccount(a); err != nil {
			return sum, err
		}
		// UpsertAccount deliberately never writes the sync overrides, so a
		// restore onto an existing account has to set them itself (nil included:
		// the dump records "inherit global" too).
		if err := s.SetAccountSyncOverrides(a.Name, a.SyncIntervalMin, a.MaxPerSync, a.SyncMonths, a.BodyCache); err != nil {
			return sum, err
		}
		sum.Accounts++
	}
	for k, v := range e.Settings {
		if err := s.setSetting(k, v); err != nil {
			return sum, err
		}
		sum.Settings++
	}
	for _, t := range e.Tokens {
		var expiry time.Time
		if t.Expiry != "" {
			expiry, _ = time.Parse(time.RFC3339, t.Expiry)
		}
		if err := s.SaveToken(t.Account, StoredToken{Access: t.Access, Refresh: t.Refresh, Expiry: expiry}); err != nil {
			return sum, err
		}
		sum.Tokens++
	}
	ruleIDs, err := s.ruleIDsByName()
	if err != nil {
		return sum, err
	}
	for _, r := range e.Filters {
		if id, ok := ruleIDs[r.Account+"\x00"+r.Name]; ok {
			r.ID = id
			err = s.UpdateRule(r)
		} else {
			err = s.CreateRule(&r) // appended to the end of the position order
		}
		if err != nil {
			return sum, err
		}
		sum.Filters++
	}
	sigIDs, err := s.signatureIDsByName()
	if err != nil {
		return sum, err
	}
	for _, se := range e.Signatures {
		sig := se.Signature
		sig.ID = sigIDs[sig.Account+"\x00"+sig.Name] // 0 = insert
		if err := s.UpsertSignature(&sig); err != nil {
			return sum, err
		}
		for _, addr := range se.Identities {
			if err := s.SetIdentitySignature(addr, sig.ID); err != nil {
				return sum, err
			}
		}
		sum.Signatures++
	}
	tpls, err := s.ListTemplates()
	if err != nil {
		return sum, err
	}
	tplIDs := map[string]int64{}
	for _, t := range tpls {
		tplIDs[t.Name] = t.ID
	}
	for _, t := range e.Templates {
		t.ID = tplIDs[t.Name] // 0 = insert
		if err := s.UpsertTemplate(&t); err != nil {
			return sum, err
		}
		sum.Templates++
	}
	saved, err := s.ListSavedSearches()
	if err != nil {
		return sum, err
	}
	have := map[string]bool{}
	for _, ss := range saved {
		have[ss.Name+"\x00"+ss.Query] = true
	}
	for _, ss := range e.SavedSearches {
		if have[ss.Name+"\x00"+ss.Query] { // saved_searches has no unique key
			continue
		}
		if err := s.SaveSearch(ss.Name, ss.Query); err != nil {
			return sum, err
		}
		sum.Searches++
	}
	for _, addr := range e.TrustedSenders {
		if err := s.SetSenderAllowsExternal(addr, true); err != nil {
			return sum, err
		}
		sum.Senders++
	}
	for _, l := range e.Labels {
		// ponytail: last write wins over whatever a re-sync put there. Fine for a
		// restore; make it a union if server-side label edits ever need to survive
		// importing an older dump.
		res, err := s.DB.Exec(`UPDATE messages SET labels = ? WHERE account = ? AND message_id = ?`,
			l.Labels, l.Account, l.MessageID)
		if err != nil {
			return sum, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			sum.Labels++
		}
	}
	return sum, nil
}

// ruleIDsByName maps "account\x00name" to rule id, the merge key for filters.
func (s *Store) ruleIDsByName() (map[string]int64, error) {
	rules, err := s.ListRules()
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, r := range rules {
		out[r.Account+"\x00"+r.Name] = r.ID
	}
	return out, nil
}

// signatureIDsByName maps "account\x00name" to signature id, the merge key for
// signatures.
func (s *Store) signatureIDsByName() (map[string]int64, error) {
	sigs, err := s.ListSignatures()
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, sig := range sigs {
		out[sig.Account+"\x00"+sig.Name] = sig.ID
	}
	return out, nil
}
