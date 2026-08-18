// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// keyPrefix is the wire-format version marker. The full key is
// "mimuxlic1.<payload-b64url>.<sig-b64url>" — see docs; pro/ verifies it offline
// and this service is the only thing that ever signs one.
const keyPrefix = "mimuxlic1"

const (
	planAnnual    = "annual"
	planPerpetual = "perpetual"
)

// licencePayload is the signed JSON body. Field order and names are the wire
// format; do not reorder for taste, the verifier unmarshals the exact bytes
// that were signed but humans read this to compare against the spec.
type licencePayload struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Plan      string `json:"plan"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt *int64 `json:"exp"`
	Watermark string `json:"watermark"`
}

func newLicenceID() string {
	return "lic_" + strings.ToLower(rand.Text())
}

// signLicence marshals the payload once and signs those exact bytes. The
// verifier never re-marshals, so the bytes encoded here are the contract.
func signLicence(key ed25519.PrivateKey, p licencePayload) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(key, b)
	return keyPrefix + "." +
		base64.RawURLEncoding.EncodeToString(b) + "." +
		base64.RawURLEncoding.EncodeToString(sig), nil
}

// parseLicenceKey reads back a key this service issued, so a renewal can see
// what the current one already covers. The signature is not checked: we made it,
// and the key never leaves our own database before coming back here.
func parseLicenceKey(key string) (licencePayload, error) {
	var p licencePayload
	parts := strings.Split(key, ".")
	if len(parts) != 3 || parts[0] != keyPrefix {
		return p, fmt.Errorf("not a %s key", keyPrefix)
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	return p, nil
}

// newLicence builds the payload for a plan. Annual expires in a year; perpetual
// never expires but is watermarked with the version it entitles you to.
func newLicence(email, plan, watermark string, now time.Time) licencePayload {
	p := licencePayload{
		ID:        newLicenceID(),
		Email:     email,
		Plan:      plan,
		IssuedAt:  now.Unix(),
		Watermark: watermark,
	}
	if plan == planAnnual {
		exp := now.AddDate(1, 0, 0).Unix()
		p.ExpiresAt = &exp
	}
	return p
}

// parseSigningKey accepts the base64 (std alphabet, padded or not) encoding of
// the 64-byte ed25519.PrivateKey.
func parseSigningKey(s string) (ed25519.PrivateKey, error) {
	s = strings.TrimSpace(s)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil {
		return nil, fmt.Errorf("LICENCE_SIGNING_KEY_B64: not base64: %w", err)
	}
	if len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("LICENCE_SIGNING_KEY_B64: want %d bytes, got %d", ed25519.PrivateKeySize, len(b))
	}
	return ed25519.PrivateKey(b), nil
}

// genkey prints a fresh keypair. Run once, ever: the private half goes in the
// VPS env as LICENCE_SIGNING_KEY_B64, the public half is baked into pro/.
func genkey() {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	fmt.Printf("LICENCE_SIGNING_KEY_B64=%s\n", base64.RawStdEncoding.EncodeToString(priv))
	fmt.Printf("public key (pro/ licencePubKeyB64)=%s\n", base64.RawStdEncoding.EncodeToString(pub))
}
