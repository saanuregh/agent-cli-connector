// Command agentd serves the agent library over JSON-RPC 2.0 on standard
// input and output, one message per line, so a program in any language can
// drive Claude Code, Codex, Pi, and OpenCode by running agentd as a child
// process. It exits when standard input closes, or on an interrupt or
// termination signal, after closing every session.
//
//	agentd [-log off|debug|info|warn|error]
//
// -log writes the library's diagnostics to standard error; the default is
// off. An error that ends agentd goes to standard error too.
//
// protocol.schema.json, next to this file, describes every method,
// notification, and type.
package main

//go:generate go run ../../internal/protocolgen -out protocol.schema.json -ts ../../clients/typescript/src/protocol.ts -py ../../clients/python/agentd/protocol.py

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/saanuregh/agent-cli-connector/internal/agentd"
)

func main() {
	level := flag.String("log", "off", "log level for diagnostics on standard error: off, debug, info, warn, or error")
	flag.Parse()
	logger, err := newLogger(*level)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentd:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agentd.Serve(ctx, os.Stdin, os.Stdout, agentd.Config{Providers: agentd.Defaults(), Logger: logger}); err != nil {
		fmt.Fprintln(os.Stderr, "agentd:", err)
		os.Exit(1)
	}
}

// newLogger returns a text logger on standard error at level, or nil for
// off.
func newLogger(level string) (*slog.Logger, error) {
	if level == "off" {
		return nil, nil
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("-log %q: want off, debug, info, warn, or error", level)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}
