//go:build pro

// SPDX-License-Identifier: AGPL-3.0-only

package main

// `mimux mail` — the terminal client, a thin HTTP client of a running mimux's
// REST API. Pro-only like the API it consumes; see pro/cli.go.
import "github.com/mattmezza/mimux/pro"

func init() { subcommands["mail"] = pro.RunCLI }
