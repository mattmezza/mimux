//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

// `mimux licence status` — what this install's licence is, and whether the API
// is answering right now. Exit 0 when it is, 1 when it is not, so a monitoring
// script is one line.
//
// It reads the same config and the same database the server does, but opens the
// store read-only: WAL lets it run alongside a live mimux, and nothing here
// writes (the trial's start row is the server's to stamp, not this command's).

// RunLicence is the `mimux licence [status]` subcommand. version and buildDate
// are the running build's -ldflags values, which cmd/mimux owns.
func RunLicence(args []string, version, buildDate string) int {
	if len(args) > 1 || (len(args) == 1 && args[0] != "status") {
		fmt.Fprintln(os.Stderr, "usage: mimux licence status")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mimux licence:", err)
		return 1
	}
	cfg.Version, cfg.BuildDate = version, buildDate

	var st *store.Store
	if _, serr := os.Stat(cfg.DB.Path); serr == nil {
		st, err = store.OpenReadOnly(cfg.DB.Path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mimux licence:", err)
			return 1
		}
		defer func() { _ = st.Close() }()
	}
	return licenceReport(os.Stdout, cfg, st, time.Now())
}

// licenceReport prints the status and returns the process exit code. Separated
// from RunLicence so the reporting — the part with the rules in it — is
// testable without a process.
func licenceReport(w io.Writer, cfg *config.Config, st *store.Store, now time.Time) int {
	s := evaluateLicence(cfg, st, now)
	var b strings.Builder
	fmt.Fprintf(&b, "status:    %s\n", s.Line)
	fmt.Fprintf(&b, "key:       %s\n", map[string]string{
		"env":  "MIMUX_LICENCE_KEY",
		"db":   "saved in Settings → Licence",
		"none": "none configured",
	}[s.Source])
	fmt.Fprintf(&b, "build:     %s\n", buildLabel(cfg.Version))
	if built, ok := parseBuildDate(cfg.BuildDate); ok {
		fmt.Fprintf(&b, "released:  %s\n", day(built))
	} else {
		fmt.Fprintf(&b, "released:  unknown (locally built — coverage dates do not apply)\n")
	}
	if p := s.Payload; p != nil {
		fmt.Fprintf(&b, "plan:      %s\n", p.Plan)
		fmt.Fprintf(&b, "email:     %s\n", maskEmail(p.Email))
		fmt.Fprintf(&b, "issued:    %s\n", day(time.Unix(p.IssuedAt, 0)))
		if p.ExpiresAt != nil {
			exp := time.Unix(*p.ExpiresAt, 0).UTC()
			fmt.Fprintf(&b, "expires:   %s\n", day(exp))
			if in := now.Sub(exp); in > 0 {
				fmt.Fprintf(&b, "grace:     day %d of %d\n", int(in/(24*time.Hour))+1, graceDays)
			}
		} else {
			fmt.Fprintf(&b, "expires:   never\n")
		}
		if p.Plan == planPerpetual && p.CoveredUntil > 0 {
			until := time.Unix(p.CoveredUntil, 0).UTC()
			covered := "covers this build"
			if built, ok := parseBuildDate(cfg.BuildDate); ok && built.After(until) {
				covered = "DOES NOT cover this build"
			}
			fmt.Fprintf(&b, "covered:   builds released up to %s (%s)\n", day(until), covered)
		}
		if p.Watermark != "" {
			covered := "covers this build"
			switch {
			case p.Plan != planPerpetual:
				covered = "informational — annual licences are not version-limited"
			case p.CoveredUntil > 0:
				covered = "informational — the covered: date is what is enforced"
			case buildAfterWatermark(cfg.Version, p.Watermark):
				covered = "DOES NOT cover this build"
			}
			fmt.Fprintf(&b, "watermark: %s (%s)\n", p.Watermark, covered)
		}
	} else if start, ok := trialStart(st); ok {
		fmt.Fprintf(&b, "trial:     started %s\n", day(start))
	} else {
		fmt.Fprintf(&b, "trial:     not started (the server stamps it on its first pro boot)\n")
	}
	fmt.Fprintf(&b, "api:       %s\n", apiLabel(s))
	_, _ = io.WriteString(w, b.String())
	if s.Allowed {
		return 0
	}
	return 1
}

func buildLabel(v string) string {
	if _, _, ok := parseVersion(v); !ok {
		return v + " (unversioned build — version rules do not apply)"
	}
	return v
}

func apiLabel(s licenceState) string {
	if !s.Allowed {
		return "paused — " + s.Message
	}
	if s.Warning != "" {
		return "answering (" + s.Warning + ")"
	}
	return "answering"
}
