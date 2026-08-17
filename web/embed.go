// SPDX-License-Identifier: AGPL-3.0-only
// Package web embeds templates and static assets.
package web

import "embed"

//go:embed templates static
var FS embed.FS
