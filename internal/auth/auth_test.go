// SPDX-License-Identifier: AGPL-3.0-only
package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("unexpected format: %s", h)
	}
	if !VerifyPassword("hunter2hunter2", h) {
		t.Error("correct password rejected")
	}
	if VerifyPassword("wrong", h) {
		t.Error("wrong password accepted")
	}
	if VerifyPassword("hunter2hunter2", "garbage") {
		t.Error("garbage hash accepted")
	}
}

func TestCSRF(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := CSRF(ok)

	// GET passes without token
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Errorf("GET blocked: %d", rec.Code)
	}

	// POST without token blocked
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST without token = %d, want 403", rec.Code)
	}

	// POST with matching cookie+form token passes
	form := url.Values{CSRFField: {"tok123"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookie, Value: "tok123"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("valid POST = %d, want 200", rec.Code)
	}

	// POST with mismatched token blocked
	req = httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookie, Value: "other"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("mismatched POST = %d, want 403", rec.Code)
	}

	// POST with header token (htmx) passes
	req = httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-CSRF-Token", "tok123")
	req.AddCookie(&http.Cookie{Name: CSRFCookie, Value: "tok123"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("header-token POST = %d, want 200", rec.Code)
	}
}
