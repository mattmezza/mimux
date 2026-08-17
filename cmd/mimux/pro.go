//go:build pro

// SPDX-License-Identifier: AGPL-3.0-only

package main

// The one line that pulls the ELv2 pro layer into the binary. Guarded by the
// same build tag as pro/ itself, so `make build` links none of it — see
// LICENSING.md.
import _ "github.com/mattmezza/mimux/pro"
