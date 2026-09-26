package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/client"
)

const (
	hookMaxPayload  = 1 << 20
	hookReadTimeout = 2 * time.Second
	hookSendTimeout = 2 * time.Second
)

// newHookCmd is `fleet hook`, run by agent CLIs from their hooks. It must
// never disturb the agent: it prints nothing, ignores every error and always
// exits 0 (agents feed hook stdout to the model, and some treat non-zero
// exits as "block this action"). Flags are parsed by hand so even a bad
// command line cannot make cobra print usage.
func newHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "hook --agent ID --adapter A <event> [payload]",
		Short:              "Internal: deliver an agent hook event to the local daemon",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			runHook(args, os.Stdin)
			return nil
		},
	}
}

// hookArgs is the parsed `fleet hook` command line.
type hookArgs struct {
	agent, adapter, event string
	payload               []byte // from the positional argument, if any
	hasPayload            bool
}

// parseHookArgs accepts --agent/--adapter as "--k v", "--k=v" or with a
// single dash, anywhere on the line. Unknown flags are ignored.
func parseHookArgs(args []string) hookArgs {
	var h hookArgs
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		k, v, hasV := strings.Cut(strings.TrimLeft(a, "-"), "=")
		var dst *string
		switch k {
		case "agent":
			dst = &h.agent
		case "adapter":
			dst = &h.adapter
		default:
			continue
		}
		if !hasV && i+1 < len(args) {
			i++
			v = args[i]
		}
		*dst = v
	}
	if len(pos) > 0 {
		h.event = pos[0]
	}
	if len(pos) > 1 {
		h.payload, h.hasPayload = []byte(pos[1]), true
	}
	return h
}

func runHook(args []string, stdin *os.File) {
	defer func() { _ = recover() }()
	h := parseHookArgs(args)
	if h.agent == "" || h.event == "" {
		return
	}
	payload := h.payload
	if !h.hasPayload && stdin != nil && !term.IsTerminal(int(stdin.Fd())) {
		payload = readWithTimeout(stdin, hookMaxPayload, hookReadTimeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookSendTimeout)
	defer cancel()
	c, err := client.DialLocal(ctx)
	if err != nil {
		return
	}
	defer c.Close()
	_ = c.Hook(ctx, &fleetv1.HookEvent{AgentId: h.agent, Adapter: h.adapter, Event: h.event, Payload: payload})
}

// readWithTimeout reads r to EOF (at most max bytes), giving up after
// timeout with whatever arrived so far.
func readWithTimeout(r io.Reader, max int64, timeout time.Duration) []byte {
	var buf syncBuffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, io.LimitReader(r, max))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
	return buf.bytes()
}

// syncBuffer is a bytes.Buffer safe for one writer and concurrent readers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}
