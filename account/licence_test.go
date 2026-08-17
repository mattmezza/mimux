// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestSignVerifyRoundTrip pins the wire format. It deliberately verifies the way
// pro/ does — split, decode, ed25519.Verify on the payload bytes — instead of
// calling a helper from this package, so a change here that pro/ would reject
// fails the build.
func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1755400000, 0)

	for _, plan := range []string{planAnnual, planPerpetual} {
		p := newLicence("customer@example.com", plan, "v0.19", now)
		key, err := signLicence(priv, p)
		if err != nil {
			t.Fatalf("%s: sign: %v", plan, err)
		}

		parts := strings.Split(key, ".")
		if len(parts) != 3 {
			t.Fatalf("%s: want 3 parts, got %d in %q", plan, len(parts), key)
		}
		if parts[0] != "mimuxlic1" {
			t.Errorf("%s: prefix = %q, want mimuxlic1", plan, parts[0])
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("%s: payload b64: %v", plan, err)
		}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatalf("%s: sig b64: %v", plan, err)
		}
		if !ed25519.Verify(pub, payload, sig) {
			t.Fatalf("%s: signature does not verify", plan)
		}

		var got licencePayload
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", plan, err)
		}
		if got.Email != "customer@example.com" || got.Plan != plan || got.Watermark != "v0.19" {
			t.Errorf("%s: payload = %+v", plan, got)
		}
		if !strings.HasPrefix(got.ID, "lic_") {
			t.Errorf("%s: id = %q, want lic_ prefix", plan, got.ID)
		}
		if got.IssuedAt != now.Unix() {
			t.Errorf("%s: iat = %d, want %d", plan, got.IssuedAt, now.Unix())
		}
		switch plan {
		case planAnnual:
			if got.ExpiresAt == nil || *got.ExpiresAt != now.AddDate(1, 0, 0).Unix() {
				t.Errorf("annual: exp = %v, want one year out", got.ExpiresAt)
			}
		case planPerpetual:
			if got.ExpiresAt != nil {
				t.Errorf("perpetual: exp = %v, want null", *got.ExpiresAt)
			}
		}

		// Tampering must break verification: the signature covers the payload
		// bytes, not the base64 of them.
		bad := append([]byte(nil), payload...)
		bad = []byte(strings.Replace(string(bad), "customer@example.com", "attacker@example.com", 1))
		if ed25519.Verify(pub, bad, sig) {
			t.Fatalf("%s: edited payload still verifies", plan)
		}
	}
}

// TestPerpetualExpJSONIsNull guards the one field the format spells out as
// nullable — an omitempty here would silently change the wire format.
func TestPerpetualExpJSONIsNull(t *testing.T) {
	b, err := json.Marshal(newLicence("a@b.co", planPerpetual, "v0.19", time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"exp":null`) {
		t.Fatalf("perpetual payload = %s, want exp:null", b)
	}
}

func TestParseSigningKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, enc := range map[string]string{
		"padded": base64.StdEncoding.EncodeToString(priv),
		"raw":    base64.RawStdEncoding.EncodeToString(priv),
	} {
		got, err := parseSigningKey(enc + "\n")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !got.Equal(priv) {
			t.Errorf("%s: key mismatch", name)
		}
	}
	if _, err := parseSigningKey("not base64 at all !!"); err == nil {
		t.Error("garbage key accepted")
	}
	if _, err := parseSigningKey(base64.StdEncoding.EncodeToString([]byte("too short"))); err == nil {
		t.Error("short key accepted")
	}
}
