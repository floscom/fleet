package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"fleet/internal/config"
	"fleet/internal/discovery"
	"fleet/internal/identity"
)

// Timeouts used by Connect.
const (
	connectDialTimeout = 5 * time.Second
	connectBrowseTime  = 2 * time.Second
)

// IsLocal reports whether target selects the local daemon.
func IsLocal(target string) bool { return target == "" || target == "local" }

// OpenServers opens the known-servers store (config.Path("servers.json")).
func OpenServers() (*identity.ServerStore, error) {
	return identity.OpenServerStore(config.Path("servers.json"))
}

// LoadDevice loads (or creates) this machine's device key under FLEET_HOME.
func LoadDevice() (*identity.Device, error) {
	h, err := config.EnsureHome()
	if err != nil {
		return nil, err
	}
	return identity.LoadOrCreateDevice(h)
}

// FindServer looks up a known server by name, id or id prefix, or by its
// last address.
func FindServer(store *identity.ServerStore, target string) (identity.KnownServer, bool) {
	if k, ok := store.Find(target); ok {
		return k, true
	}
	addr := HostPort(target)
	for _, k := range store.List() {
		if k.Address == addr {
			return k, true
		}
	}
	return identity.KnownServer{}, false
}

// Connect dials the daemon selected by target: "" (or "local") is the local
// socket; anything else names a paired server (name, id prefix or address).
// If its last known address fails, the server is looked up via mDNS and the
// stored address is updated on success.
func Connect(ctx context.Context, target string) (*Client, error) {
	if IsLocal(target) {
		return DialLocal(ctx)
	}
	store, err := OpenServers()
	if err != nil {
		return nil, err
	}
	known, ok := FindServer(store, target)
	if !ok {
		return nil, fmt.Errorf("unknown server %q (pair first with: fleet connect <addr> --code XXXX-XXXX; list with: fleet servers)", target)
	}
	dev, err := LoadDevice()
	if err != nil {
		return nil, fmt.Errorf("load device key: %w", err)
	}
	var firstErr error
	if known.Address != "" {
		c, err := dialWithTimeout(ctx, known.Address, known, dev)
		if err == nil {
			return c, nil
		}
		if !retryable(ctx, err) {
			return nil, err
		}
		firstErr = err
	}
	found, err := discovery.Browse(ctx, connectBrowseTime)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	for _, f := range found {
		if f.ServerID != known.ID {
			continue
		}
		for _, ip := range f.Addrs {
			addr := net.JoinHostPort(ip, strconv.Itoa(f.Port))
			c, err := dialWithTimeout(ctx, addr, known, dev)
			if err != nil {
				if !retryable(ctx, err) {
					return nil, err
				}
				continue
			}
			known.Address = addr
			_ = store.Put(known) // best effort; the connection is good either way
			return c, nil
		}
	}
	if firstErr == nil {
		firstErr = errors.New("no address known")
	}
	return nil, fmt.Errorf("cannot reach %s: %w", known.Name, firstErr)
}

func dialWithTimeout(ctx context.Context, addr string, known identity.KnownServer, dev *identity.Device) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, connectDialTimeout)
	defer cancel()
	return DialRemote(ctx, addr, known, dev)
}

// retryable reports whether trying another address might help: network
// failures and certificate mismatches (the address may now belong to another
// host) yes; daemon refusals and cancellation no.
func retryable(ctx context.Context, err error) bool {
	var e *Error
	return ctx.Err() == nil && !errors.As(err, &e)
}
