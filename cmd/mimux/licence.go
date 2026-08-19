//go:build pro

// SPDX-License-Identifier: AGPL-3.0-only

package main

// `mimux licence status` — what the pro layer makes of this install's licence.
// Pro-only, like the enforcement it reports on; see pro/licence_cli.go.
import "github.com/mattmezza/mimux/pro"

func init() {
	subcommands["licence"] = func(args []string) int { return pro.RunLicence(args, version, buildDate) }
}
