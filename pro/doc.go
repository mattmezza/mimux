// SPDX-License-Identifier: LicenseRef-Elastic-2.0
//
// Package pro is the mimux commercial automation layer: REST API, MCP server,
// webhooks, AI compose.
//
// Everything in this directory is licensed under the Elastic Licence 2.0
// (pro/LICENSE), NOT the AGPL-3.0 that covers the rest of the repository. See
// LICENSING.md at the root.
//
// Every other file here carries "//go:build pro", so the default build excludes
// this package from the build graph entirely. This file has no build tag purely
// so that `go build ./...` and `go vet ./...` do not fail on a directory with no
// buildable Go files; it contains no code.
package pro
