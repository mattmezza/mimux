// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/mattmezza/mimux/internal/mail"
)

func (s *Server) handleMessageHeaders(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	raw, _, err := s.mail.Headers(r.Context(), msg)
	if err != nil {
		http.Error(w, "message headers unavailable", http.StatusBadGateway)
		return
	}
	s.renderPartial(w, "message_headers", map[string]any{"Raw": raw})
}

func (s *Server) handleMessageRaw(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	raw, err := s.mail.Raw(r.Context(), msg)
	if err != nil {
		http.Error(w, "message source unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", mail.MessageFilename(msg.Subject, msg.ID)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	// #nosec G705 -- raw message bytes are deliberately served as a download;
	// message/rfc822, attachment disposition, and nosniff prevent HTML rendering.
	_, _ = w.Write(raw)
}
