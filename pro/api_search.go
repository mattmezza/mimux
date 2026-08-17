//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/search"
	"github.com/mattmezza/mimux/internal/store"
)

const searchLimit = 100
const searchLimitMax = 500
const searchCacheTTL = 10 * time.Minute

type searchRequest struct {
	Query    string `json:"query"`
	Scope    string `json:"scope"` // all|account|folder (default all)
	Account  string `json:"account"`
	FolderID int64  `json:"folder_id"`
	Mode     string `json:"mode"` // local|deep (default local)
	Limit    int    `json:"limit"`
}

// handleSearch runs a query in the client's own search language (from:, to:,
// subject:, is:unread, has:attachment, before:/after:, …). local searches the
// synced SQLite index synchronously; deep asks the IMAP servers themselves and
// runs as an async job — poll GET /search/jobs/{id} for results.
func (a *api) handleSearch(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	q := search.Parse(req.Query)
	if q.Empty() {
		apiError(w, http.StatusBadRequest, "invalid_request", "query is empty.")
		return
	}
	// ParseScope defaults to account (right for the UI, which always has an
	// account in context); the API defaults to all so a bare query means
	// "everywhere".
	if req.Scope == "" {
		if req.FolderID != 0 {
			req.Scope = string(search.ScopeFolder)
		} else if req.Account != "" {
			req.Scope = string(search.ScopeAccount)
		} else {
			req.Scope = string(search.ScopeAll)
		}
	}
	scope := search.ParseScope(req.Scope)
	limit := req.Limit
	if limit <= 0 {
		limit = searchLimit
	}
	if limit > searchLimitMax {
		limit = searchLimitMax
	}

	switch req.Mode {
	case "", "local":
		msgs, err := a.store.SearchLocal(q, scope, req.Account, req.FolderID, limit)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "internal", "Search failed.")
			return
		}
		out := make([]messageJSON, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, toMessageJSON(m))
		}
		writeList(w, out, "")
	case "deep":
		job := a.startDeepSearch(req.Query, q, scope, req.Account, req.FolderID)
		writeJSONStatus(w, http.StatusAccepted, a.jobs.view(job, a.store))
	default:
		apiError(w, http.StatusBadRequest, "invalid_request", "mode must be local or deep.")
	}
}

func (a *api) handleSearchJob(w http.ResponseWriter, r *http.Request) {
	job := a.jobs.get(chi.URLParam(r, "id"))
	if job == nil {
		apiError(w, http.StatusNotFound, "not_found", "No such search job (jobs are kept in memory and expire on restart).")
		return
	}
	writeJSON(w, a.jobs.view(job, a.store))
}

// --- deep-search jobs ---

// searchJob is one async IMAP search. ids is only read once status is "done".
type searchJob struct {
	ID        string
	Query     string
	StartedAt time.Time

	mu     sync.Mutex
	status string // running|done
	ids    []int64
}

// jobTable holds recent jobs in memory.
// ponytail: capped ring of 32, single user; jobs die with the process, which
// the 404 message tells the caller about. Persist if agents ever need more.
type jobTable struct {
	mu    sync.Mutex
	m     map[string]*searchJob
	order []string
}

const jobMax = 32

func newJobTable() *jobTable { return &jobTable{m: map[string]*searchJob{}} }

func (t *jobTable) add(j *searchJob) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.order) >= jobMax {
		delete(t.m, t.order[0])
		t.order = t.order[1:]
	}
	t.m[j.ID] = j
	t.order = append(t.order, j.ID)
}

func (t *jobTable) get(id string) *searchJob {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[id]
}

// view renders a job for the API, resolving result ids to messages when done.
func (t *jobTable) view(j *searchJob, st *store.Store) map[string]any {
	j.mu.Lock()
	status := j.status
	ids := append([]int64(nil), j.ids...)
	j.mu.Unlock()
	out := map[string]any{
		"id": j.ID, "status": status, "query": j.Query,
		"started_at": j.StartedAt.UTC().Format(time.RFC3339),
	}
	if status == "done" {
		msgs, _ := st.MessagesByIDs(ids)
		results := make([]messageJSON, 0, len(msgs))
		for _, m := range msgs {
			results = append(results, toMessageJSON(m))
		}
		out["results"] = results
	}
	return out
}

// startDeepSearch creates a job and fans the IMAP search out per account, the
// same shape as the HTML client's server search: per-account 10s cap, envelope
// caching for hits, result-id set cached so an identical re-query is instant.
func (a *api) startDeepSearch(raw string, q *search.SearchQuery, scope search.Scope, account string, folderID int64) *searchJob {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	job := &searchJob{ID: hex.EncodeToString(buf), Query: raw, StartedAt: time.Now(), status: "running"}
	a.jobs.add(job)

	// A recent identical query answers from the cache without touching IMAP.
	if ids, ok := a.store.SearchCacheGet(store.QueryHash(raw, scope, account, folderID), searchCacheTTL); ok {
		job.mu.Lock()
		job.status, job.ids = "done", ids
		job.mu.Unlock()
		a.searchCompleted(job, len(ids))
		return job
	}

	accounts := a.mail.SearchAccounts(scope, account, folderID)
	go func() {
		crit := mail.Criteria(q)
		results := make(chan []int64, len(accounts))
		for _, acct := range accounts {
			go func(acct string) {
				ctx, cancel := context.WithTimeout(context.Background(), mail.SearchTimeout)
				defer cancel()
				var folders []store.Folder
				if scope == search.ScopeFolder {
					if f, _ := a.store.FolderByID(folderID); f != nil {
						folders = []store.Folder{*f}
					}
				} else {
					folders = a.mail.SearchFolders(acct, q)
				}
				var ids []int64
				for _, f := range folders {
					uids, err := a.mail.SearchFolder(ctx, acct, &f, crit)
					if err != nil || len(uids) == 0 {
						continue
					}
					fids, _ := a.mail.CacheEnvelopes(ctx, acct, &f, uids)
					ids = append(ids, fids...)
				}
				results <- ids
			}(acct)
		}
		var all []int64
		for range accounts {
			all = append(all, <-results...)
		}
		_ = a.store.SearchCachePut(store.QueryHash(raw, scope, account, folderID), all)
		job.mu.Lock()
		job.status, job.ids = "done", all
		job.mu.Unlock()
		a.searchCompleted(job, len(all))
	}()
	return job
}

// searchCompleted fires the search.completed webhook. A deep search is the one
// API call that finishes long after its response was written, so it is the one
// an agent most wants pushed rather than polled.
func (a *api) searchCompleted(job *searchJob, results int) {
	if a.hooks.fire("search.completed", map[string]any{
		"job_id": job.ID, "query": job.Query, "results": results,
	}) {
		a.sendSoon() // off the request path: the cached branch runs inline
	}
}
