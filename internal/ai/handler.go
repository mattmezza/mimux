package ai

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/internal/mail"
)

// Routes returns a mountable router exposing the AI-assist JSON endpoints. CSRF
// is enforced by the global auth.CSRF middleware already applied in
// server.Handler(), so no CSRF check is duplicated here. Clients are built per
// request (newClient) so key/model/prefs edited in Settings take effect
// without a restart.
func Routes(newClient func() *Client) chi.Router {
	r := chi.NewRouter()

	// POST /ai/options — reply directions for the message being replied to.
	r.Post("/options", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.PostFormValue("context")
		if ctx == "" {
			httpErr(w, http.StatusBadRequest, "No message to reply to.")
			return
		}
		opts, err := newClient().Options(r.Context(), ctx)
		if err != nil {
			slog.Error("ai options", "err", err)
			httpErr(w, http.StatusBadGateway, ErrMessage(err))
			return
		}
		writeJSON(w, map[string]any{"options": opts})
	})

	// POST /ai/draft — generate a full draft (fresh compose or a chosen reply
	// direction), formatted for the compose mode.
	r.Post("/draft", func(w http.ResponseWriter, r *http.Request) {
		mode := normalizeMode(r.PostFormValue("mode"))
		direction := r.PostFormValue("direction")
		if direction == "" {
			httpErr(w, http.StatusBadRequest, "Tell the AI what to write.")
			return
		}
		wantSubject := r.PostFormValue("want_subject") == "1"
		res, err := newClient().Draft(r.Context(), mode, r.PostFormValue("context"), direction, wantSubject)
		if err != nil {
			slog.Error("ai draft", "err", err)
			httpErr(w, http.StatusBadGateway, ErrMessage(err))
			return
		}
		draft := res.Draft
		if mode == "html" {
			draft = mail.SanitizeComposeHTML(draft)
		}
		writeJSON(w, map[string]any{"draft": draft, "subject": res.Subject})
	})

	// POST /ai/refine — rewrite the current draft (shorter|formal|friendlier).
	r.Post("/refine", func(w http.ResponseWriter, r *http.Request) {
		mode := normalizeMode(r.PostFormValue("mode"))
		text := r.PostFormValue("text")
		if text == "" {
			httpErr(w, http.StatusBadRequest, "Nothing to refine yet.")
			return
		}
		draft, err := newClient().Refine(r.Context(), mode, text, r.PostFormValue("action"))
		if err != nil {
			slog.Error("ai refine", "err", err)
			httpErr(w, http.StatusBadGateway, ErrMessage(err))
			return
		}
		if mode == "html" {
			draft = mail.SanitizeComposeHTML(draft)
		}
		writeJSON(w, map[string]any{"draft": draft})
	})

	return r
}

func normalizeMode(m string) string {
	switch m {
	case "html", "markdown", "plain":
		return m
	default:
		return "plain"
	}
}

// ErrMessage turns an AI failure into the user-facing sentence every AI feature
// shows (exported for the summary strip, which renders server-side). Only the
// two actionable cases get their own wording; everything else is transient as
// far as the user is concerned — chat already retried it — so the message says
// so instead of casting doubt on a key that is fine.
func ErrMessage(err error) string {
	switch {
	case errors.Is(err, ErrDisabled):
		return "AI is off — add an OpenRouter key in Settings → AI."
	case errors.Is(err, ErrAuth):
		return "OpenRouter rejected your API key — check it in Settings → AI."
	}
	return "The AI service is busy or unreachable — try again in a moment."
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("ai: encode json", "err", err)
	}
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
