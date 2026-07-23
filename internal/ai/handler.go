package ai

import (
	"html/template"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/web"
)

// Routes returns a mountable router exposing POST /ai/compose and
// POST /ai/reply. CSRF is enforced by the global auth.CSRF middleware
// already applied in server.Handler(), so no CSRF check is duplicated here.
func Routes(newClient func() *Client) chi.Router {
	tmpl := template.Must(template.ParseFS(web.FS,
		"templates/partials/ai_compose.html",
		"templates/partials/ai_reply.html",
	))

	r := chi.NewRouter()
	r.Post("/compose", func(w http.ResponseWriter, r *http.Request) {
		client := newClient()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		topic := r.PostFormValue("topic")
		if topic == "" {
			renderErr(w, tmpl, "ai_compose_error", "Give the AI a topic to write about.")
			return
		}
		draft, err := client.Compose(r.Context(), topic)
		if err != nil {
			slog.Error("ai compose", "err", err)
			renderErr(w, tmpl, "ai_compose_error", "AI is unavailable. Check your OpenRouter key or try again.")
			return
		}
		render(w, tmpl, "ai_compose_result", map[string]any{"Draft": draft})
	})
	r.Post("/reply", func(w http.ResponseWriter, r *http.Request) {
		client := newClient()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		threadContext := r.PostFormValue("context")
		instructions := r.PostFormValue("instructions")
		if threadContext == "" {
			renderErr(w, tmpl, "ai_reply_error", "No thread context to reply to.")
			return
		}
		draft, err := client.Reply(r.Context(), threadContext, instructions)
		if err != nil {
			slog.Error("ai reply", "err", err)
			renderErr(w, tmpl, "ai_reply_error", "AI is unavailable. Check your OpenRouter key or try again.")
			return
		}
		render(w, tmpl, "ai_reply_result", map[string]any{"Draft": draft})
	})
	return r
}

func render(w http.ResponseWriter, tmpl *template.Template, name string, data any) {
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("ai: render", "template", name, "err", err)
	}
}

func renderErr(w http.ResponseWriter, tmpl *template.Template, name, msg string) {
	render(w, tmpl, name, map[string]any{"Error": msg})
}
