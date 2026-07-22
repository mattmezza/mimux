package server

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/internal/auth"
	"github.com/mattmezza/sm/internal/mail"
	"github.com/mattmezza/sm/internal/search"
	"github.com/mattmezza/sm/internal/store"
)

const searchLimit = 100
const serverCacheTTL = 10 * time.Minute

// searchPill is one parsed operator rendered as a removable chip.
type searchPill struct {
	Label string // e.g. "from:alice"
	Query string // the full query with this term removed, for re-running
}

// searchGroup groups results under an account header (scope=all); Account is
// "" for the single ungrouped list.
type searchGroup struct {
	Account string
	Msgs    []store.Message
}

// handleSearch renders the full local results view (Enter / results page).
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	scope := search.ParseScope(r.URL.Query().Get("scope"))
	folderID, _ := strconv.ParseInt(r.URL.Query().Get("folder"), 10, 64)
	account := r.URL.Query().Get("account")

	// Account scope needs an account name; derive it from the current folder
	// when the client only sent a folder id.
	if scope == search.ScopeAccount && account == "" && folderID != 0 {
		if f, _ := s.store.FolderByID(folderID); f != nil {
			account = f.Account
		}
	}

	q := search.Parse(raw)
	var msgs []store.Message
	if !q.Empty() {
		msgs, _ = s.store.SearchLocal(q, scope, account, folderID, searchLimit)
		if r.URL.Query().Get("save") != "0" {
			_ = s.store.AddHistory(raw)
		}
	}
	csrf := auth.EnsureCSRF(w, r, s.secure)
	s.renderPartial(w, "search_results", s.searchData(csrf, raw, q, scope, account, folderID, msgs, false))
}

// handleSearchSuggest renders the instant typeahead dropdown: top local hits, or
// recent history when the box is empty.
func (s *Server) handleSearchSuggest(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	scope := search.ParseScope(r.URL.Query().Get("scope"))
	folderID, _ := strconv.ParseInt(r.URL.Query().Get("folder"), 10, 64)
	account := r.URL.Query().Get("account")

	data := map[string]any{"Q": raw, "Scope": string(scope), "Account": account, "Folder": folderID}
	if raw == "" {
		hist, _ := s.store.ListHistory(10)
		data["History"] = hist
	} else {
		q := search.Parse(raw)
		msgs, _ := s.store.SearchLocal(q, scope, account, folderID, 8)
		data["Msgs"] = msgs
		data["Terms"] = q.TextTerms()
	}
	s.renderPartial(w, "search_suggest", data)
}

// searchData assembles the template payload shared by the results view.
func (s *Server) searchData(csrf, raw string, q *search.SearchQuery, scope search.Scope, account string, folderID int64, msgs []store.Message, serverRan bool) map[string]any {
	groups := groupResults(msgs, scope)
	return map[string]any{
		"CSRF":      csrf,
		"Q":         raw,
		"Scope":     string(scope),
		"Account":   account,
		"Folder":    folderID,
		"Pills":     pills(q),
		"Terms":     q.TextTerms(),
		"Groups":    groups,
		"Count":     len(msgs),
		"HasResults": len(msgs) > 0,
		"Grouped":   scope == search.ScopeAll,
		"ServerRan": serverRan,
	}
}

func groupResults(msgs []store.Message, scope search.Scope) []searchGroup {
	if scope != search.ScopeAll {
		return []searchGroup{{Msgs: msgs}}
	}
	order := []string{}
	byAcct := map[string]*searchGroup{}
	for _, m := range msgs {
		g := byAcct[m.Account]
		if g == nil {
			g = &searchGroup{Account: m.Account}
			byAcct[m.Account] = g
			order = append(order, m.Account)
		}
		g.Msgs = append(g.Msgs, m)
	}
	out := make([]searchGroup, 0, len(order))
	for _, a := range order {
		out = append(out, *byAcct[a])
	}
	return out
}

// pills builds the removable operator chips (bare words are not shown as pills).
func pills(q *search.SearchQuery) []searchPill {
	var out []searchPill
	for i, t := range q.Terms {
		if t.Op == "text" {
			continue
		}
		out = append(out, searchPill{Label: t.String(), Query: q.Without(i)})
	}
	return out
}

// --- server (deep) search ---

// handleServerSearch kicks off IMAP SEARCH across the relevant accounts and
// folders. Results stream back over SSE (search-started/results/done); this
// returns the per-account spinner scaffold immediately.
func (s *Server) handleServerSearch(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.FormValue("q"))
	scope := search.ParseScope(r.FormValue("scope"))
	folderID, _ := strconv.ParseInt(r.FormValue("folder"), 10, 64)
	account := r.FormValue("account")
	q := search.Parse(raw)
	if q.Empty() {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Cheap re-run: serve a recent identical query straight from the cache.
	if ids, ok := s.store.SearchCacheGet(store.QueryHash(raw, scope, account, folderID), serverCacheTTL); ok {
		msgs, _ := s.store.MessagesByIDs(ids)
		s.renderPartial(w, "search_server_status", map[string]any{
			"Cached": true, "Rows": template.HTML(s.renderRows(msgs, q.TextTerms())), "Count": len(msgs), // #nosec G203 -- renderRows output is template-generated, escaped per field.
		})
		return
	}

	accounts := s.searchAccounts(scope, account, folderID)
	go s.runServerSearch(raw, q, scope, account, folderID, accounts)

	s.renderPartial(w, "search_server_status", map[string]any{"Accounts": accounts})
}

// searchAccounts resolves which account names participate in a server search.
func (s *Server) searchAccounts(scope search.Scope, account string, folderID int64) []string {
	switch scope {
	case search.ScopeFolder:
		if f, _ := s.store.FolderByID(folderID); f != nil {
			return []string{f.Account}
		}
		return nil
	case search.ScopeAccount:
		if account != "" {
			return []string{account}
		}
		if f, _ := s.store.FolderByID(folderID); f != nil {
			return []string{f.Account}
		}
	}
	out := make([]string, 0, len(s.cfg.Accounts))
	for _, a := range s.cfg.Accounts {
		out = append(out, a.Name)
	}
	return out
}

// runServerSearch fans out one goroutine per account (each capped at 10s),
// emits SSE events, caches envelopes for uncached hits, and stores the id set.
func (s *Server) runServerSearch(raw string, q *search.SearchQuery, scope search.Scope, account string, folderID int64, accounts []string) {
	crit := mail.Criteria(q)
	var allIDs []int64
	results := make(chan []int64, len(accounts))

	for _, acct := range accounts {
		go func(acct string) {
			ctx, cancel := context.WithTimeout(context.Background(), mail.SearchTimeout)
			defer cancel()
			s.mail.Broadcast("search-started", jsonData(map[string]any{"account": acct}))

			var folders []store.Folder
			if scope == search.ScopeFolder {
				if f, _ := s.store.FolderByID(folderID); f != nil {
					folders = []store.Folder{*f}
				}
			} else {
				folders = s.mail.SearchFolders(acct, q)
			}

			var ids []int64
			for _, f := range folders {
				uids, err := s.mail.SearchFolder(ctx, acct, &f, crit)
				if err != nil || len(uids) == 0 {
					continue
				}
				fids, _ := s.mail.CacheEnvelopes(ctx, acct, &f, uids)
				msgs, _ := s.store.MessagesByIDs(fids)
				if len(msgs) > 0 {
					ids = append(ids, fids...)
					s.mail.Broadcast("search-results", jsonData(map[string]any{
						"account": acct,
						"html":    s.renderRows(msgs, q.TextTerms()),
					}))
				}
			}
			s.mail.Broadcast("search-done", jsonData(map[string]any{"account": acct, "count": len(ids)}))
			results <- ids
		}(acct)
	}
	for range accounts {
		allIDs = append(allIDs, <-results...)
	}
	_ = s.store.SearchCachePut(store.QueryHash(raw, scope, account, folderID), allIDs)
}

// renderRows renders a batch of result rows to an HTML string for SSE delivery.
func (s *Server) renderRows(msgs []store.Message, terms []string) string {
	var b bytes.Buffer
	for i := range msgs {
		_ = s.tmpl["inbox"].ExecuteTemplate(&b, "search_row", map[string]any{
			"Msg": msgs[i], "Terms": terms, "Grouped": false,
		})
	}
	return b.String()
}

func jsonData(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// --- history + saved searches ---

func (s *Server) handleHistoryClear(w http.ResponseWriter, r *http.Request) {
	_ = s.store.ClearHistory()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleSaveSearch(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	query := strings.TrimSpace(r.FormValue("query"))
	if name != "" && query != "" {
		_ = s.store.SaveSearch(name, query)
	}
	s.renderPartial(w, "saved_searches", map[string]any{"Saved": s.savedSearches()})
}

func (s *Server) handleDeleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	_ = s.store.DeleteSavedSearch(id)
	s.renderPartial(w, "saved_searches", map[string]any{"Saved": s.savedSearches()})
}

func (s *Server) savedSearches() []store.SavedSearch {
	saved, _ := s.store.ListSavedSearches()
	return saved
}

// --- template helpers ---

// highlight HTML-escapes s, then wraps case-insensitive matches of any term in
// <mark>. Escaping happens first, so a subject like "<script>" can never inject
// markup — the only tags in the output are the <mark>s we add.
func highlight(s string, terms []string) template.HTML {
	esc := template.HTMLEscapeString(s)
	for _, raw := range terms {
		t := template.HTMLEscapeString(strings.TrimSpace(raw))
		if t == "" {
			continue
		}
		esc = markAll(esc, t)
	}
	return template.HTML(esc) // #nosec G203 -- input was HTML-escaped above; only our <mark> tags are raw.
}

// markAll wraps every case-insensitive occurrence of needle in haystack with
// <mark>…</mark>, preserving the original casing of each match.
func markAll(haystack, needle string) string {
	var b strings.Builder
	lowH, lowN := strings.ToLower(haystack), strings.ToLower(needle)
	for {
		i := strings.Index(lowH, lowN)
		if i < 0 {
			b.WriteString(haystack)
			break
		}
		b.WriteString(haystack[:i])
		b.WriteString(`<mark class="bg-amber-300/30 text-inherit rounded-sm">`)
		b.WriteString(haystack[i : i+len(needle)])
		b.WriteString(`</mark>`)
		haystack, lowH = haystack[i+len(needle):], lowH[i+len(needle):]
	}
	return b.String()
}
