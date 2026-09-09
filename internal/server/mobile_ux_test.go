// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	webassets "github.com/mattmezza/mimux/web"
)

func assetText(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(webassets.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBodyReloadsReplaceIframeHistory(t *testing.T) {
	app := assetText(t, "static/js/app.js")
	if !strings.Contains(app, "contentWindow.location.replace(url)") {
		t.Fatal("loadBody does not use replace semantics")
	}
	for _, name := range []string{"templates/partials/message_detail.html", "templates/partials/thread_detail.html", "templates/partials/translate_bar.html"} {
		body := assetText(t, name)
		if strings.Contains(body, ".src='/messages/") || strings.Contains(body, "frame.src = u.pathname") {
			t.Errorf("%s still appends iframe history", name)
		}
	}
}

func TestReadingSkeletonSurvivesPaneSwaps(t *testing.T) {
	inbox := assetText(t, "templates/pages/inbox.html")
	if !strings.Contains(inbox, `id="reading-skeleton-single"`) || !strings.Contains(inbox, `id="reading-skeleton-thread"`) {
		t.Fatal("persistent skeleton templates missing")
	}
	app := assetText(t, "static/js/app.js")
	if !strings.Contains(app, `e.detail.target.replaceChildren(template.content.cloneNode(true))`) {
		t.Fatal("reading requests do not install the skeleton immediately")
	}
}

func TestPDFJSIsVendoredAndLazy(t *testing.T) {
	for _, name := range []string{"static/js/pdf.min.mjs", "static/js/pdf.worker.min.mjs", "static/js/pdfjs-LICENSE.txt", "static/js/pdfjs-README.txt"} {
		if _, err := fs.Stat(webassets.FS, name); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	app := assetText(t, "static/js/app.js")
	if !strings.Contains(app, `await import("/static/js/pdf.min.mjs")`) || !strings.Contains(app, `pdf.worker.min.mjs`) {
		t.Fatal("PDF preview is not lazy-loading the vendored renderer")
	}
	for _, want := range []string{
		`const MAX_PAGES = 100, MAX_CANVASES = 6, MAX_PIXELS = 12_000_000, MAX_EDGE = 4096`,
		`new IntersectionObserver`, `state.loadingTask?.destroy()`, `task.cancel()`,
		`htmx:beforeCleanupElement`,
		`Open in accessible PDF viewer`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("bounded PDF preview missing %q", want)
		}
	}
	if strings.Contains(app, `for (let number = 1; number <= pdf.numPages; number++)`) {
		t.Fatal("PDF preview still eagerly renders every page")
	}
	sw := assetText(t, "static/js/sw.js")
	if strings.Contains(sw, `"/static/js/pdf.min.mjs"`) {
		t.Fatal("PDF renderer must remain out of eager service-worker precache")
	}
}

func TestDayGroupsAreRemovedWhenTheirRowsDisappear(t *testing.T) {
	app := assetText(t, "static/js/app.js")
	for _, want := range []string{
		`document.querySelectorAll("#message-list-items [data-day-group]").forEach(cleanupDayGroup)`,
		`cleanupDayGroup(group)`, `sub?.remove()`,
		`group.querySelector(":scope > ul > li[data-message-row]")`,
	} {
		if !strings.Contains(app, want) { t.Errorf("day-group cleanup missing %q", want) }
	}
}

func TestPDFModulesServedAsJavaScript(t *testing.T) {
	s := serverWith(t, nil, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/js/pdf.min.mjs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("module status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Fatalf("module Content-Type = %q", got)
	}
}

func TestUnreadFilterKeepsThreadContext(t *testing.T) {
	css := assetText(t, "static/css/app.css")
	if !strings.Contains(css, `:not([data-mid]):not([data-unread])`) {
		t.Fatal("unread filter still hides read thread sub-rows")
	}
	if !strings.Contains(css, `[data-filter="starred"] #message-list-items li[data-message-row]:not([data-starred])`) {
		t.Fatal("starred per-message filter changed")
	}
}
