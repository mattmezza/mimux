package translate

import (
	"html/template"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/internal/store"
	"github.com/mattmezza/sm/web"
)

// Routes returns a mountable router exposing POST /translate. CSRF is
// enforced by the global auth.CSRF middleware already applied in
// server.Handler(), so no CSRF check is duplicated here.
func Routes(st *store.Store, newClient func() *Client) chi.Router {
	tmpl := template.Must(template.ParseFS(web.FS,
		"templates/partials/translate_result.html",
	))

	r := chi.NewRouter()
	r.Post("/", func(w http.ResponseWriter, r *http.Request) {
		client := newClient()
		text := r.PostFormValue("text")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if text == "" {
			renderError(w, tmpl, "Nothing to translate.")
			return
		}

		key := store.TranslationCacheKey(text, client.Target)
		if translated, lang, ok, err := st.TranslationCached(key); err == nil && ok {
			renderResult(w, tmpl, translated, lang)
			return
		}

		translated, lang, err := client.Translate(r.Context(), text)
		if err != nil {
			slog.Error("translate", "err", err)
			renderError(w, tmpl, "Translation is unavailable. Check your Translate API key or try again.")
			return
		}
		if err := st.SaveTranslation(key, translated, lang); err != nil {
			slog.Error("translate: cache save", "err", err)
		}
		renderResult(w, tmpl, translated, lang)
	})
	return r
}

func renderResult(w http.ResponseWriter, tmpl *template.Template, text, lang string) {
	if lang == "" {
		lang = "unknown"
	}
	data := map[string]any{"Text": text, "DetectedLang": lang}
	if err := tmpl.ExecuteTemplate(w, "translate_result", data); err != nil {
		slog.Error("translate: render", "err", err)
	}
}

func renderError(w http.ResponseWriter, tmpl *template.Template, msg string) {
	if err := tmpl.ExecuteTemplate(w, "translate_error", map[string]any{"Error": msg}); err != nil {
		slog.Error("translate: render error", "err", err)
	}
}
