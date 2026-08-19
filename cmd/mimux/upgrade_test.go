// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// A pro build upgrading itself into a free one would silently delete somebody's
// paid-for automation layer, so the flavour derivation gets a check.
func TestAssetName(t *testing.T) {
	for _, tc := range []struct{ version, goos, goarch, want string }{
		{"v0.21.3", "linux", "amd64", "mimux-linux-amd64"},
		{"v0.21.3-pro", "linux", "arm64", "mimux-pro-linux-arm64"},
		{"v0.21.3-pro", "darwin", "amd64", "mimux-pro-darwin-amd64"},
		{"dev", "darwin", "arm64", "mimux-darwin-arm64"},
	} {
		if got := assetName(tc.version, tc.goos, tc.goarch); got != tc.want {
			t.Errorf("assetName(%q, %q, %q) = %q, want %q", tc.version, tc.goos, tc.goarch, got, tc.want)
		}
	}
}

func TestNormaliseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v0.21.3":     "v0.21.3",
		"v0.21.3-pro": "v0.21.3",
		"dev":         "dev",
	} {
		if got := normaliseVersion(in); got != want {
			t.Errorf("normaliseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerifyChecksum(t *testing.T) {
	sums := []byte("aa11  mimux-linux-arm64\nbb22 *mimux-linux-amd64\n\n")
	if err := verifyChecksum("mimux-linux-amd64", "BB22", sums); err != nil {
		t.Errorf("matching sum rejected (case-insensitively): %v", err)
	}
	if err := verifyChecksum("mimux-linux-arm64", "dead", sums); err == nil {
		t.Error("a wrong checksum was accepted")
	}
	if err := verifyChecksum("mimux-darwin-arm64", "aa11", sums); err == nil {
		t.Error("an asset with no entry was accepted")
	}
}

// The whole point of writing beside the target and renaming: the file that ends
// up in place is the complete new binary, executable, and no temporary debris
// is left behind.
func TestReplaceExecutable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mimux")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil { // #nosec G306 -- pretending to be an executable
		t.Fatal(err)
	}

	if err := replaceExecutable(target, []byte("new binary")); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(target) // #nosec G304 -- t.TempDir() path
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new binary" {
		t.Errorf("target = %q, want the new bytes", b)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("replacement is not executable: %v", fi.Mode())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

// A container is a layer, not a place to patch: upgrading in one is undone by
// the next restart, so the command has to bail out before downloading anything.
func TestRunUpgradeRefusesInContainer(t *testing.T) {
	t.Setenv("MIMUX_IN_CONTAINER", "1")
	if got := runUpgrade(nil, "v0.21.3"); got != 1 {
		t.Errorf("runUpgrade in a container = %d, want 1", got)
	}
}

// Likewise a dev build: there is no release it corresponds to, and replacing it
// would throw away whatever was just compiled.
func TestRunUpgradeRefusesDevBuild(t *testing.T) {
	if got := runUpgrade(nil, "dev"); got != 1 {
		t.Errorf("runUpgrade on a dev build = %d, want 1", got)
	}
}

// Guards the release side of the contract: the checksums.txt the workflow
// writes with `sha256sum` is the format verifyChecksum reads.
func TestVerifyChecksumAcceptsRealSha256sumOutput(t *testing.T) {
	sum := sha256.Sum256([]byte("binary"))
	hexsum := hex.EncodeToString(sum[:])
	if err := verifyChecksum("mimux-linux-amd64", hexsum, []byte(hexsum+"  mimux-linux-amd64\n")); err != nil {
		t.Errorf("sha256sum-format line rejected: %v", err)
	}
}
