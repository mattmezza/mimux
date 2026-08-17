//go:build pro

// SPDX-License-Identifier: AGPL-3.0-only

package main

// `mimux mcp` — the stdio MCP bridge, for clients that can't speak streamable
// HTTP themselves. Pro-only like the endpoint it proxies to; see pro/mcp_bridge.go.
import "github.com/mattmezza/mimux/pro"

func init() { subcommands["mcp"] = pro.RunMCPBridge }
