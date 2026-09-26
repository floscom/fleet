package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"fleet/internal/client"
)

// detachKey is Ctrl-\ (FS).
const detachKey = 0x1c

// termReset undoes modes a remote tmux client may leave on when the stream
// ends abruptly: mouse reporting, bracketed paste, hidden cursor.
const termReset = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?25h"

// leaveAltScreen is only sent when the stream died without tmux restoring
// the screen itself.
const leaveAltScreen = "\x1b[?1049l"

func newAttachCmd() *cobra.Command {
	var readOnly bool
	cmd := &cobra.Command{
		Use:   "attach <agent>",
		Short: "Attach to an agent's terminal (detach with Ctrl-\\)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, c *client.Client) error {
				return attach(ctx, c, args[0], readOnly)
			})
		},
	}
	cmd.Flags().BoolVarP(&readOnly, "read-only", "r", false, "watch without sending keystrokes")
	return cmd
}

// attach streams an agent's terminal through the protocol until the user
// detaches, the agent exits or the connection drops. The local terminal is
// always restored.
func attach(ctx context.Context, c *client.Client, agent string, readOnly bool) error {
	inFd, outFd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !term.IsTerminal(inFd) || !term.IsTerminal(outFd) {
		return errors.New("attach needs an interactive terminal")
	}
	cols, rows, err := term.GetSize(outFd)
	if err != nil {
		cols, rows = 0, 0
	}
	t, err := c.Attach(ctx, agent, client.AttachOptions{Cols: clamp16(cols), Rows: clamp16(rows), ReadOnly: readOnly})
	if err != nil {
		return err
	}
	mode := ""
	if readOnly {
		mode = " read-only"
	}
	fmt.Fprintf(os.Stderr, "[attached to %s%s; detach with Ctrl-\\]\n", agent, mode)

	old, err := term.MakeRaw(inFd)
	if err != nil {
		_ = t.Detach(context.Background())
		return fmt.Errorf("raw mode: %w", err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			_ = term.Restore(inFd, old)
		}
	}
	defer restore()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	detach := make(chan struct{})
	go pumpInput(t, readOnly, detach)

	userDetached := false
loop:
	for {
		select {
		case b, ok := <-t.Output():
			if !ok {
				break loop
			}
			if _, err := os.Stdout.Write(b); err != nil {
				userDetached = true // our side is gone; detach cleanly
				break loop
			}
		case <-winch:
			if w, h, err := term.GetSize(outFd); err == nil {
				_ = t.Resize(clamp16(w), clamp16(h))
			}
		case <-detach:
			userDetached = true
			break loop
		case <-ctx.Done():
			userDetached = true
			break loop
		}
	}
	if userDetached {
		dctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = t.Detach(dctx)
		cancel()
		// Let tmux's own detach sequence (screen restore) reach the terminal.
		drain(t)
	}
	reason := t.Reason()
	if reason == "connection closed" {
		os.Stdout.WriteString(leaveAltScreen)
	}
	os.Stdout.WriteString(termReset)
	restore()
	if userDetached {
		fmt.Fprintf(os.Stderr, "\n[detached from %s]\n", agent)
		return nil
	}
	fmt.Fprintf(os.Stderr, "\n[%s: %s]\n", agent, reason)
	return nil
}

// drain copies output still in flight after a detach, briefly.
func drain(t *client.Terminal) {
	timeout := time.After(300 * time.Millisecond)
	for {
		select {
		case b, ok := <-t.Output():
			if !ok {
				return
			}
			os.Stdout.Write(b)
		case <-timeout:
			return
		}
	}
}

// pumpInput forwards stdin to the terminal until the detach key is pressed
// or stdin ends (both close detach), or the terminal is gone.
func pumpInput(t *client.Terminal, readOnly bool, detach chan<- struct{}) {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		data := buf[:n]
		i := bytes.IndexByte(data, detachKey)
		if i >= 0 {
			data = data[:i]
		}
		if !readOnly && len(data) > 0 {
			if _, werr := t.Write(data); werr != nil {
				return // terminal ended; the output loop sees why
			}
		}
		if i >= 0 || err != nil {
			close(detach)
			return
		}
	}
}

func clamp16(v int) uint16 {
	switch {
	case v < 0:
		return 0
	case v > 0xffff:
		return 0xffff
	default:
		return uint16(v)
	}
}
