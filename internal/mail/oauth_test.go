package mail

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/mattmezza/mimux/internal/store"
)

func TestXOAUTH2InitialResponse(t *testing.T) {
	// The XOAUTH2 initial client response is the exact byte string
	// "user=<email>\x01auth=Bearer <token>\x01\x01".
	want := "user=me@example.com\x01auth=Bearer tok123\x01\x01"
	if got := string(xoauth2InitialResponse("me@example.com", "tok123")); got != want {
		t.Fatalf("initial response = %q, want %q", got, want)
	}

	mech, ir, err := newXOAUTH2Client("me@example.com", "tok123").Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mech != "XOAUTH2" {
		t.Errorf("mech = %q, want XOAUTH2", mech)
	}
	if string(ir) != want {
		t.Errorf("Start ir = %q, want %q", ir, want)
	}
}

// TestPersistTokenSourceRefresh verifies that an expired token is refreshed
// against the (fake) token endpoint and the refreshed token is written back to
// the store.
func TestPersistTokenSourceRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "refresh_token" || r.FormValue("refresh_token") != "r1" {
			http.Error(w, "bad refresh", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"newAT","refresh_token":"r2","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	st := testStore(t)
	oc := &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: srv.URL}}
	expired := &oauth2.Token{AccessToken: "oldAT", RefreshToken: "r1", Expiry: time.Now().Add(-time.Hour)}
	src := &persistTokenSource{
		base:    oauth2.ReuseTokenSource(expired, oc.TokenSource(t.Context(), expired)),
		st:      st,
		account: "acc",
		lastAcc: "oldAT",
	}

	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "newAT" {
		t.Errorf("access token = %q, want newAT", tok.AccessToken)
	}
	stored, err := st.GetToken("acc")
	if err != nil || stored == nil {
		t.Fatalf("GetToken: %v (stored=%v)", err, stored)
	}
	if stored.Access != "newAT" || stored.Refresh != "r2" {
		t.Errorf("persisted token = %+v, want access newAT / refresh r2", stored)
	}
}

func TestGetTokenRoundTrip(t *testing.T) {
	st := testStore(t)
	exp := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	if err := st.SaveToken("a", store.StoredToken{Access: "x", Refresh: "y", Expiry: exp}); err != nil {
		t.Fatal(err)
	}
	// A blank refresh token must not clobber the stored one.
	if err := st.SaveToken("a", store.StoredToken{Access: "x2", Refresh: "", Expiry: exp}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetToken("a")
	if err != nil || got == nil {
		t.Fatalf("GetToken: %v", err)
	}
	if got.Access != "x2" || got.Refresh != "y" || !got.Expiry.Equal(exp) {
		t.Errorf("got %+v, want access x2 / refresh y / expiry %v", got, exp)
	}
}
