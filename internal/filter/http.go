// SPDX-License-Identifier: AGPL-3.0-only
package filter

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/web"
)

// RuleStore is the persistence dependency Routes needs. *store.Store
// satisfies it structurally (see internal/store/filters.go) — filter can't
// import store directly without an import cycle (store needs the Rule
// type defined in this package).
type RuleStore interface {
	ListRules() ([]Rule, error)
	GetRule(id int64) (*Rule, error)
	CreateRule(r *Rule) error
	UpdateRule(r Rule) error
	DeleteRule(id int64) error
	ToggleRule(id int64) error
	ReorderRules(ids []int64) error
	// RecentInbox is the dry run's corpus: the newest stored inbox messages,
	// as the engine sees them. account "" means every account.
	RecentInbox(account string, limit int) ([]MessageMeta, error)
}

// dryRunScan is how much recent mail a dry run reads. Big enough to be a real
// answer on a normal mailbox, small enough to stay one indexed query.
const dryRunScan = 200

// dryRunShow is how many matches the page lists before it says "and N more".
const dryRunShow = 15

// Routes returns a chi router covering the filters management page:
// list/create/edit/delete/reorder/toggle, plus the dry run. Mount it under
// /filters behind the app's auth+CSRF middleware, e.g.:
//
//	r.Mount("/filters", filter.Routes(st, s.secure, templateFuncs, s.filtersPageData))
//
// page fills in what the shared layout needs (version, appearance, sync
// state) and the vocabulary the form offers (Accounts, Folders, Labels) —
// none of which this package can know on its own.
func Routes(rs RuleStore, secure bool, funcs template.FuncMap, page func(map[string]any)) chi.Router {
	tmpl := template.Must(template.New("").Funcs(funcs).ParseFS(web.FS,
		"templates/pages/filters.html",
		"templates/layouts/base.html",
	))
	h := &handler{rs: rs, tmpl: tmpl, secure: secure, page: page}

	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Post("/", h.create)
	r.Post("/reorder", h.reorder)
	r.Post("/{id}", h.update)
	r.Post("/{id}/delete", h.delete)
	r.Post("/{id}/toggle", h.toggle)
	return r
}

type handler struct {
	rs     RuleStore
	tmpl   *template.Template
	secure bool
	page   func(map[string]any)
}

// --- view models (carry CSRF + derived fields alongside the plain Rule) ---

type ruleView struct {
	Rule
	CSRF    string
	UpIDs   string // POST target for /filters/reorder to move this rule up; "" if already first
	DownIDs string
}

type formView struct {
	CSRF           string
	Editing        bool
	ID             int64
	Account        string
	Name           string
	Enabled        bool
	ConditionsJSON template.JS
	ActionsJSON    template.JS
}

// dryRunView is "what would this rule have done to the mail I already have".
type dryRunView struct {
	Rule    Rule
	Scanned int
	Matches []MessageMeta
	More    int
}

func blankForm(csrf string) formView {
	return formView{
		CSRF:           csrf,
		Enabled:        true,
		ConditionsJSON: `[{"field":"from","op":"contains","value":""}]`,
		ActionsJSON:    `[{"type":"mark_read","arg":""}]`,
	}
}

func editForm(csrf string, r Rule) formView {
	cond, _ := json.Marshal(r.Conditions)
	act, _ := json.Marshal(r.Actions)
	return formView{
		CSRF: csrf, Editing: true, ID: r.ID, Account: r.Account, Name: r.Name, Enabled: r.Enabled,
		// #nosec G203 -- json.Marshal escapes <, >, & as \uXXXX, so the payload cannot break out of a script context.
		ConditionsJSON: template.JS(cond), ActionsJSON: template.JS(act),
	}
}

// --- handlers ---

// list renders the whole page: the rules, optionally the create/edit form
// (?new=1 / ?edit=<id>) and optionally that rule's dry run (&test=1). One
// handler, because every one of those is the same screen in a different state
// and a POST that redirects back here is the only way state changes.
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	rules, err := h.rs.ListRules()
	if err != nil {
		slog.Error("filters: list", "err", err)
		http.Error(w, "failed to load rules", http.StatusInternalServerError)
		return
	}
	csrf := auth.EnsureCSRF(w, r, h.secure)

	ids := make([]int64, len(rules))
	for i, rule := range rules {
		ids[i] = rule.ID
	}
	views := make([]ruleView, len(rules))
	for i, rule := range rules {
		v := ruleView{Rule: rule, CSRF: csrf}
		if i > 0 {
			v.UpIDs = swappedIDs(ids, i, i-1)
		}
		if i < len(ids)-1 {
			v.DownIDs = swappedIDs(ids, i, i+1)
		}
		views[i] = v
	}

	data := map[string]any{"CSRF": csrf, "Rules": views}
	if msg := r.URL.Query().Get("err"); msg != "" {
		data["Error"] = msg
	}
	if id, err := strconv.ParseInt(r.URL.Query().Get("edit"), 10, 64); err == nil {
		if rule, err := h.rs.GetRule(id); err == nil && rule != nil {
			data["Form"] = editForm(csrf, *rule)
			if r.URL.Query().Get("test") != "" {
				data["DryRun"] = h.dryRun(*rule)
			}
		}
	} else if r.URL.Query().Get("new") != "" {
		data["Form"] = blankForm(csrf)
	}
	h.render(w, data)
}

// dryRun answers "which of my recent messages does this rule match" by running
// the same Matches the sync loop runs, over stored mail. No I/O beyond one
// read: a dry run can never mark, move or forward anything.
func (h *handler) dryRun(rule Rule) dryRunView {
	corpus, err := h.rs.RecentInbox(rule.Account, dryRunScan)
	if err != nil {
		slog.Error("filters: dry run", "err", err)
	}
	v := dryRunView{Rule: rule, Scanned: len(corpus), Matches: DryRun(rule, corpus)}
	if len(v.Matches) > dryRunShow {
		v.More = len(v.Matches) - dryRunShow
		v.Matches = v.Matches[:dryRunShow]
	}
	return v
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	rule, err := ruleFromForm(r, 0)
	if err != nil {
		h.redirectError(w, r, "/filters?new=1", err)
		return
	}
	if err := h.rs.CreateRule(&rule); err != nil {
		slog.Error("filters: create", "err", err)
		h.redirectError(w, r, "/filters?new=1", fmt.Errorf("could not save rule"))
		return
	}
	http.Redirect(w, r, "/filters", http.StatusSeeOther)
}

func (h *handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rule, err := ruleFromForm(r, id)
	back := fmt.Sprintf("/filters?edit=%d", id)
	if err != nil {
		h.redirectError(w, r, back, err)
		return
	}
	if err := h.rs.UpdateRule(rule); err != nil {
		slog.Error("filters: update", "err", err)
		h.redirectError(w, r, back, fmt.Errorf("could not save rule"))
		return
	}
	http.Redirect(w, r, "/filters", http.StatusSeeOther)
}

func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := h.rs.DeleteRule(id); err != nil {
		slog.Error("filters: delete", "err", err)
	}
	http.Redirect(w, r, "/filters", http.StatusSeeOther)
}

func (h *handler) toggle(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := h.rs.ToggleRule(id); err != nil {
		slog.Error("filters: toggle", "err", err)
	}
	http.Redirect(w, r, "/filters", http.StatusSeeOther)
}

// reorder applies the full ordered id list posted by an up/down button
// (see swappedIDs) or, in the future, a drag-and-drop client.
func (h *handler) reorder(w http.ResponseWriter, r *http.Request) {
	raw := r.PostFormValue("ids")
	var ids []int64
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			http.Error(w, "invalid ids", http.StatusBadRequest)
			return
		}
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		if err := h.rs.ReorderRules(ids); err != nil {
			slog.Error("filters: reorder", "err", err)
		}
	}
	http.Redirect(w, r, "/filters", http.StatusSeeOther)
}

// --- helpers ---

func (h *handler) render(w http.ResponseWriter, data map[string]any) {
	if h.page != nil {
		h.page(data)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "base", data); err != nil {
		slog.Error("filters: render", "err", err)
	}
}

func (h *handler) redirectError(w http.ResponseWriter, r *http.Request, path string, err error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	http.Redirect(w, r, path+sep+"err="+url.QueryEscape(err.Error()), http.StatusSeeOther) // #nosec G710 -- path is always a hardcoded local route, error text is query-escaped.
}

func idParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

// ruleFromForm builds a Rule from posted form values and validates it.
// cond_field/cond_op/cond_value and act_type/act_arg are parallel repeated
// fields, one per condition/action row (see filters.html).
func ruleFromForm(r *http.Request, id int64) (Rule, error) {
	if err := r.ParseForm(); err != nil {
		return Rule{}, fmt.Errorf("could not parse form")
	}
	fields, ops, values := r.PostForm["cond_field"], r.PostForm["cond_op"], r.PostForm["cond_value"]
	conditions := make([]Condition, 0, len(fields))
	for i := range fields {
		if i >= len(ops) || i >= len(values) {
			break
		}
		conditions = append(conditions, Condition{Field: fields[i], Op: ops[i], Value: values[i]})
	}
	types, args := r.PostForm["act_type"], r.PostForm["act_arg"]
	actions := make([]Action, 0, len(types))
	for i := range types {
		arg := ""
		// An action that takes no argument still posts its (hidden, stale) input,
		// so switching "move to Archive" to "star" would store `star Archive` and
		// show it on the page. The engine ignores the extra, the reader does not.
		if i < len(args) && NeedsArg(types[i]) {
			arg = strings.TrimSpace(args[i])
		}
		actions = append(actions, Action{Type: types[i], Arg: arg})
	}
	rule := Rule{
		ID:         id,
		Account:    strings.TrimSpace(r.PostFormValue("account")),
		Name:       strings.TrimSpace(r.PostFormValue("name")),
		Enabled:    r.PostFormValue("enabled") != "",
		Conditions: conditions,
		Actions:    actions,
	}
	if err := rule.Validate(); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

// swappedIDs returns the comma-joined id order with positions i and j
// swapped, the value posted by an up/down button to /filters/reorder.
func swappedIDs(ids []int64, i, j int) string {
	swapped := make([]string, len(ids))
	for k, id := range ids {
		swapped[k] = strconv.FormatInt(id, 10)
	}
	swapped[i], swapped[j] = swapped[j], swapped[i]
	return strings.Join(swapped, ",")
}
