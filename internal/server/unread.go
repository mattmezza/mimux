package server

import (
	"net/http"
	"strconv"
)

// handleUnread returns the total inbox unread count as a plain integer, so the
// client can show it in the browser tab title on every page.
func (s *Server) handleUnread(w http.ResponseWriter, r *http.Request) {
	n, _ := s.store.TotalInboxUnread()
	_, _ = w.Write([]byte(strconv.Itoa(n)))
}
