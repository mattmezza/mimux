// SPDX-License-Identifier: AGPL-3.0-only

package main

// `mimux upgrade` — replace this binary with the newest release of the same
// flavour. Free-side on purpose: a free install has as much right to stay
// current as a pro one, so there is no build tag on this file and both binaries
// carry the command.
//
// It shares exactly one thing with www/src/install.sh: the checksums.txt every
// release publishes. Neither can call the other, so the download-and-verify
// dance is written twice; the file they both read is what keeps them honest.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const releasesURL = "https://github.com/mattmezza/mimux/releases"

func init() {
	subcommands["upgrade"] = func(args []string) int { return runUpgrade(args, version) }
}

func runUpgrade(args []string, current string) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	check := fs.Bool("check", false, "report the latest version without installing it")
	want := fs.String("version", "", "install this exact release (default: the latest)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mimux upgrade [--check] [--version vX.Y.Z]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if where := containerHint(); where != "" {
		fmt.Fprintf(os.Stderr, `mimux upgrade: this is running inside %s, and patching a container
layer would be undone by the next restart. Pull a new image instead:

  docker pull ghcr.io/mattmezza/mimux:%s
  docker compose up -d
`, where, imageTag(current))
		return 1
	}
	// A build nobody released has no release to compare against, and replacing
	// it with a tagged one would silently throw away whatever was built locally.
	if normaliseVersion(current) == "dev" {
		fmt.Fprintln(os.Stderr, "mimux upgrade: this is a development build (mimux -version says \"dev\").")
		fmt.Fprintln(os.Stderr, "Rebuild it from source, or install a release from "+releasesURL)
		return 1
	}

	latest := *want
	if latest == "" {
		var err error
		if latest, err = latestTag(); err != nil {
			fmt.Fprintln(os.Stderr, "mimux upgrade:", err)
			return 1
		}
	}

	if *check {
		fmt.Printf("installed %s\nlatest    %s\n", current, latest)
		if normaliseVersion(current) == latest {
			return 0
		}
		fmt.Println("run `mimux upgrade` to update")
		return 1
	}
	if *want == "" && normaliseVersion(current) == latest {
		fmt.Printf("mimux %s is already the latest release.\n", current)
		return 0
	}

	asset := assetName(current, runtime.GOOS, runtime.GOARCH)
	fmt.Printf("Downloading %s %s ...\n", asset, latest)
	blob, sum, err := download(assetURL(latest, asset))
	if err != nil {
		fmt.Fprintln(os.Stderr, "mimux upgrade:", err)
		return 1
	}
	sums, _, err := download(assetURL(latest, "checksums.txt"))
	if err != nil {
		fmt.Fprintf(os.Stderr, `mimux upgrade: %v
Releases before v0.22 publish no checksums.txt, so there is nothing to verify
this download against. Download it by hand from %s if you want it anyway.
`, err, releasesURL)
		return 1
	}
	if err := verifyChecksum(asset, sum, sums); err != nil {
		fmt.Fprintln(os.Stderr, "mimux upgrade:", err)
		return 1
	}

	target, err := currentExecutable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mimux upgrade:", err)
		return 1
	}
	if err := replaceExecutable(target, blob); err != nil {
		fmt.Fprintln(os.Stderr, "mimux upgrade:", err)
		return 1
	}
	fmt.Printf("Upgraded %s: %s -> %s\n", target, current, latest)
	return 0
}

// normaliseVersion drops the -pro suffix main.version carries in pro builds, so
// a version can be compared against a release tag, which has no such suffix.
func normaliseVersion(v string) string { return strings.TrimSuffix(v, "-pro") }

// assetName derives the release asset from the running binary's own version:
// pro upgrades to pro, free to free, and neither can accidentally become the
// other.
func assetName(current, goos, goarch string) string {
	name := "mimux"
	if strings.HasSuffix(current, "-pro") {
		name = "mimux-pro"
	}
	return fmt.Sprintf("%s-%s-%s", name, goos, goarch)
}

// imageTag is the docker tag matching the running build, for the message shown
// when someone runs this inside a container.
func imageTag(current string) string {
	if strings.HasSuffix(current, "-pro") {
		return "pro"
	}
	return "latest"
}

func assetURL(tag, asset string) string {
	return fmt.Sprintf("%s/download/%s/%s", releasesURL, tag, asset)
}

// latestTag reads the tag out of the redirect GitHub serves for /releases/latest
// rather than calling the API, which is rate-limited to 60 requests an hour per
// IP for unauthenticated callers — a limit a NAT'd office could hit.
func latestTag() (string, error) {
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(releasesURL + "/latest") // #nosec G107 -- constant URL
	if err != nil {
		return "", fmt.Errorf("cannot reach %s: %w", releasesURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	loc := resp.Header.Get("Location")
	if _, tag, ok := strings.Cut(loc, "/releases/tag/"); ok && tag != "" {
		return tag, nil
	}
	return "", fmt.Errorf("could not work out the latest release from %s (got %s)", releasesURL+"/latest", resp.Status)
}

// download returns the body and its sha256. Held in memory rather than streamed
// to disk so a failed checksum never leaves a partial binary lying next to the
// real one; the binaries are tens of megabytes, which is nothing next to what
// the mail cache already costs.
func download(url string) ([]byte, string, error) {
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(url) // #nosec G107 -- built from the constant release URL
	if err != nil {
		return nil, "", fmt.Errorf("cannot download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("cannot download %s: %s", url, resp.Status)
	}
	sum := sha256.New()
	blob, err := io.ReadAll(io.TeeReader(resp.Body, sum))
	if err != nil {
		return nil, "", fmt.Errorf("cannot download %s: %w", url, err)
	}
	return blob, hex.EncodeToString(sum.Sum(nil)), nil
}

// verifyChecksum looks name up in a standard sha256sum listing and compares.
// A missing entry is as fatal as a wrong one: both mean we cannot say what we
// just downloaded.
func verifyChecksum(name, got string, checksums []byte) error {
	for _, line := range strings.Split(string(checksums), "\n") {
		want, file, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		// sha256sum writes two spaces for text mode and " *" for binary.
		if strings.TrimLeft(strings.TrimSpace(file), "*") != name {
			continue
		}
		if !strings.EqualFold(want, got) {
			return fmt.Errorf("checksum mismatch for %s: expected %s, got %s — refusing to install", name, want, got)
		}
		return nil
	}
	return fmt.Errorf("checksums.txt has no entry for %s — refusing to install", name)
}

// currentExecutable resolves symlinks, so upgrading through a
// /usr/local/bin/mimux -> /opt/mimux/mimux link replaces the real file rather
// than turning the link into a regular one.
func currentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot find this binary on disk: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// replaceExecutable writes the new binary beside the old one and renames it
// over the top. Same directory so the rename is atomic (a cross-device rename
// is a copy, which can be interrupted half-written); renaming over a running
// executable is fine on Unix, where the running process keeps the old inode.
func replaceExecutable(target string, blob []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".mimux-upgrade-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot write to %s. Re-run with sudo:\n  sudo %s upgrade\nor upgrade through whatever installed it (a package manager, or docker pull)", dir, target)
		}
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename below succeeds
	if _, err := tmp.Write(blob); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o755); err != nil { // #nosec G302 -- an executable everyone may run, like every other binary in bin/
		return err
	}
	if err := os.Rename(name, target); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot replace %s. Re-run with sudo:\n  sudo %s upgrade", target, target)
		}
		return err
	}
	return nil
}

// containerHint names the container runtime this is running under, or "" for
// the host. Both files are created by the runtime, not by the image, so this
// stays true for images nobody built here.
func containerHint() string {
	if os.Getenv("MIMUX_IN_CONTAINER") != "" {
		return "a container"
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker"
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return "podman"
	}
	return ""
}
