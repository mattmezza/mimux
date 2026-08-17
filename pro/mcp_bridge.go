//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The stdio bridge: `mimux mcp` speaks MCP on stdin/stdout and forwards every
// tool call to a *running* mimux's /api/mcp endpoint. It never opens the
// database — one server process owns that — so it is safe to run while the
// server is up, which is the whole point: stdio-only MCP clients (editors,
// desktop apps) get the same tools the HTTP endpoint serves.
//
// Configuration is two env vars, because an MCP client's config block is
// exactly a command plus an env map:
//
//	MIMUX_URL   base URL of the running mimux (default http://localhost:8083)
//	MIMUX_TOKEN an API token from Settings → API (required)

// RunMCPBridge is the `mimux mcp` subcommand. Returns a process exit code.
func RunMCPBridge(_ []string) int {
	url := os.Getenv("MIMUX_URL")
	if url == "" {
		url = "http://localhost:8083"
	}
	token := os.Getenv("MIMUX_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "mimux mcp: set MIMUX_TOKEN to an API token from Settings → API")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runBridge(ctx, url, token); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "mimux mcp:", err)
		return 1
	}
	return 0
}

// bearerTransport adds the Authorization header to every forwarded request.
type bearerTransport struct{ token string }

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}

func runBridge(ctx context.Context, url, token string) error {
	client := mcp.NewClient(&mcp.Implementation{Name: "mimux-stdio-bridge"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url + "/api/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token: token}},
		// The remote is stateless: a standalone GET stream would just 405.
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return fmt.Errorf("connecting to %s/api/mcp: %w (is mimux running, and is the token valid?)", url, err)
	}
	defer func() { _ = session.Close() }()

	proxy, err := buildProxy(ctx, session)
	if err != nil {
		return err
	}
	return proxy.Run(ctx, &mcp.StdioTransport{})
}

// buildProxy mirrors the remote session's tools and resources onto a local
// server, forwarding calls verbatim. Schemas pass through untouched — the
// remote already validated its own contract.
func buildProxy(ctx context.Context, session *mcp.ClientSession) (*mcp.Server, error) {
	s := mcp.NewServer(&mcp.Implementation{Name: "mimux", Title: "mimux mail (stdio bridge)"}, nil)

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("listing remote tools: %w", err)
	}
	for _, t := range tools.Tools {
		t := t
		s.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return session.CallTool(ctx, &mcp.CallToolParams{Name: t.Name, Arguments: req.Params.Arguments})
		})
	}

	resources, err := session.ListResources(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("listing remote resources: %w", err)
	}
	for _, res := range resources.Resources {
		s.AddResource(res, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return session.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
		})
	}
	return s, nil
}
