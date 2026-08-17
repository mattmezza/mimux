package server

import (
	"net/http"
	"strings"

	"github.com/mattmezza/mimux/internal/store"
)

// Web Push subscription endpoints (Settings → Notifications). The browser mints
// a subscription at its vendor's push service and posts the three fields here;
// the server keeps them so it can encrypt to that device later.
//
// They're form-encoded rather than JSON on purpose: the CSRF middleware reads
// the posted token with r.PostFormValue, so a form body is the shape that
// already works end to end.

// handlePushSubscribe stores (or refreshes) this browser's subscription.
func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	endpoint := strings.TrimSpace(r.PostFormValue("endpoint"))
	p256dh := strings.TrimSpace(r.PostFormValue("p256dh"))
	auth := strings.TrimSpace(r.PostFormValue("auth"))
	// The endpoint is a URL this server will POST to, so it is a trust
	// boundary: only ever accept an https one from the push service.
	if !strings.HasPrefix(endpoint, "https://") || p256dh == "" || auth == "" {
		http.Error(w, "invalid subscription", http.StatusBadRequest)
		return
	}
	if err := s.store.SavePushSub(store.PushSub{
		Endpoint: endpoint, P256dh: p256dh, Auth: auth,
		Label: deviceLabel(r.UserAgent()),
	}); err != nil {
		http.Error(w, "Couldn't save the subscription — please try again.", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePushUnsubscribe forgets one device: the toggle turned off here, a
// remove button on another device's row, or a sign-out.
func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeletePushSub(r.PostFormValue("endpoint")); err != nil {
		http.Error(w, "Couldn't remove the device — please try again.", http.StatusInternalServerError)
		return
	}
	s.handlePushDevices(w, r)
}

// handlePushDevices renders the registered-device list. Both the remove button
// and the client-side subscribe flow swap it in, so the list always reflects
// what the server actually holds.
func (s *Server) handlePushDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.ListPushSubs()
	if err != nil {
		http.Error(w, "Couldn't read the device list.", http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, "push_devices", map[string]any{"Devices": devices})
}

// handleNotifyTest fans a test notification out over every configured transport
// so the user can prove the setup works without waiting for mail.
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	s.mail.NotifyTest()
	w.WriteHeader(http.StatusNoContent)
}

// deviceLabel is a short human hint for the Settings device list. The full
// User-Agent is noise; the browser and platform are what tell two devices
// apart.
func deviceLabel(ua string) string {
	name := "Unknown browser"
	switch {
	case strings.Contains(ua, "Firefox/"):
		name = "Firefox"
	case strings.Contains(ua, "Edg/"):
		name = "Edge"
	case strings.Contains(ua, "Chrome/"):
		name = "Chrome"
	case strings.Contains(ua, "Safari/"):
		name = "Safari"
	}
	switch {
	case strings.Contains(ua, "iPhone"):
		name += " on iPhone"
	case strings.Contains(ua, "iPad"):
		name += " on iPad"
	case strings.Contains(ua, "Android"):
		name += " on Android"
	case strings.Contains(ua, "Macintosh"):
		name += " on Mac"
	case strings.Contains(ua, "Windows"):
		name += " on Windows"
	case strings.Contains(ua, "Linux"):
		name += " on Linux"
	}
	return name
}
