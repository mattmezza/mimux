// SPDX-License-Identifier: AGPL-3.0-only

// Package ext is the boundary between the AGPL client and separately licensed
// add-ons — today only the ELv2 pro layer in pro/.
//
// It exists as its own package for one reason: so that pro/ can bind to the
// client without importing internal/server. That is enforced by
// `make verify-boundary`, not by discipline.
//
// The point of the rule: when a pro handler needs something that currently only
// exists as a private method on *server.Server, it cannot reach for it. The
// logic has to move down into internal/mail or internal/store, with the HTML
// handler calling the same code. Shared operations end up in the domain layer
// because a real caller needed them there, rather than because someone guessed
// in advance which operations an API would want.
package ext

import (
	"net/http"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// Extension is a route group contributed by a build-tagged add-on. Pattern is
// mounted on the root router, outside the session-cookie auth group:
// extensions are machine-facing and bring their own auth.
type Extension struct {
	Pattern string
	Handler http.Handler
}

// Deps is what an extension gets to work with: the client's domain layer,
// without the HTTP and template plumbing wrapped around it.
//
// Concrete types, deliberately. There is exactly one implementation of each and
// add-ons link into the same binary, so an interface here would be indirection
// with a single implementor on both sides.
// NOTE: struct, not interface — add an interface when a second
// implementation or a real test double needs one.
type Deps struct {
	Mail  *mail.Manager
	Store *store.Store
	Cfg   *config.Config
}

var registered []func(Deps) Extension

// Register is called from an init() in a build-tagged package. Not
// concurrency-safe and does not need to be: registration happens during package
// initialisation, before main runs.
func Register(f func(Deps) Extension) { registered = append(registered, f) }

// All returns the registered extensions in registration order. Empty in the
// free build, where nothing calls Register.
func All() []func(Deps) Extension { return registered }
