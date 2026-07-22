package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// attachmentView is one row in the reading-pane attachments strip.
type attachmentView struct {
	Part string // dot-joined MIME path, e.g. "2" or "1.2"
	Name string
	Size string
	Kind string // image | pdf | text | other (drives inline preview)
}

// handleAttachments lazy-loads a message's attachment strip (hx-get on open).
func (s *Server) handleAttachments(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	atts, err := s.mail.Attachments(r.Context(), msg)
	if err != nil {
		// Render nothing rather than an error banner — attachments are secondary.
		s.renderPartial(w, "attachments", map[string]any{"MsgID": msg.ID, "Attachments": nil})
		return
	}
	views := make([]attachmentView, 0, len(atts))
	for _, a := range atts {
		views = append(views, attachmentView{
			Part: partPathString(a.Part),
			Name: a.Filename,
			Size: humanSize(a.Size),
			Kind: attachmentKind(a.MediaType),
		})
	}
	s.renderPartial(w, "attachments", map[string]any{"MsgID": msg.ID, "Attachments": views})
}

// handleAttachment streams one attachment part. Inline by default (for preview);
// ?dl=1 forces a download. Served under a locked-down CSP + sandbox so a
// malicious HTML attachment can't run script in our origin.
func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	part := parsePartPath(chi.URLParam(r, "part"))
	if len(part) == 0 {
		http.NotFound(w, r)
		return
	}
	data, mediaType, filename, err := s.mail.Attachment(r.Context(), msg, part)
	if err != nil {
		http.Error(w, "attachment unavailable", http.StatusBadGateway)
		return
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if filename == "" {
		filename = "attachment"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; media-src 'self'; object-src 'none'; sandbox")
	disp := "inline"
	if r.URL.Query().Get("dl") != "" {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disp, sanitizeFilename(filename)))
	_, _ = w.Write(data)
}

// partPathString joins an IMAP part path for a URL ([1,2] -> "1.2").
func partPathString(p []int) string {
	parts := make([]string, len(p))
	for i, n := range p {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// parsePartPath is the inverse; returns nil on any invalid segment.
func parsePartPath(s string) []int {
	if s == "" {
		return nil
	}
	segs := strings.Split(s, ".")
	out := make([]int, 0, len(segs))
	for _, seg := range segs {
		n, err := strconv.Atoi(seg)
		if err != nil || n < 1 {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func attachmentKind(mediaType string) string {
	mt := strings.ToLower(mediaType)
	switch {
	case strings.HasPrefix(mt, "image/"):
		return "image"
	case mt == "application/pdf":
		return "pdf"
	case strings.HasPrefix(mt, "text/"):
		return "text"
	default:
		return "other"
	}
}

// sanitizeFilename strips characters that could break the Content-Disposition
// header or path (CR/LF/quote/backslash/slash).
func sanitizeFilename(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '"', '\\', '/':
			return '_'
		}
		return r
	}, name)
}

func humanSize(n uint32) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
