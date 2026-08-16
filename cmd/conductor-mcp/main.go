// Command conductor-mcp is the MCP gateway an agent harness mounts.
//
// It speaks JSON-RPC over stdio and translates every tool call into an HTTP request to the
// control plane. It holds no state and makes no decisions of its own — see DESIGN.md §7.2
// and internal/mcp for why that separation matters.
//
// Mount it in Claude Code:
//
//	{"mcpServers": {"conductor": {"command": "conductor-mcp",
//	  "env": {"CONDUCTOR_TOKEN": "...", "CONDUCTOR_PROJECT": "my-project"}}}}
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/adamburan/conductor/internal/mcp"
)

func main() {
	fs := flag.NewFlagSet("conductor-mcp", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "control plane URL (default: saved credentials or CONDUCTOR_ENDPOINT)")
	token := fs.String("token", "", "bearer token (default: saved credentials or CONDUCTOR_TOKEN)")
	project := fs.String("project", "", "project id or slug (default: CONDUCTOR_PROJECT)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `conductor-mcp — Conductor coordination tools over MCP

Reads JSON-RPC from stdin and writes to stdout. Diagnostics go to stderr, so they never
corrupt the protocol stream.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := mcp.New(mcp.Options{
		Endpoint: *endpoint,
		Token:    *token,
		Project:  *project,
	})

	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "conductor-mcp:", err)
		os.Exit(1)
	}
}
