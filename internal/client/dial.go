package client

import (
	"context"
	"crypto/hmac"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/config"
	"fleet/internal/identity"
)

// DefaultPort is the daemon's default TCP port.
const DefaultPort = 7420

// dialTimeout bounds TCP connection setup when ctx has no deadline.
const dialTimeout = 10 * time.Second

var (
	// ErrNotRunning means no daemon answers on the local socket.
	ErrNotRunning = errors.New("fleet daemon is not running (start it with: fleet start)")
	// ErrFingerprintMismatch means the server's certificate is not the pinned one.
	ErrFingerprintMismatch = errors.New("server certificate does not match the paired server id")
	// ErrServerProof means the server could not prove it knows the pairing
	// code: someone may be intercepting the connection.
	ErrServerProof = errors.New("server failed to prove the pairing code (possible interception); not trusting it")
	// ErrInvalidCode means the pairing code is malformed.
	ErrInvalidCode = errors.New("invalid pairing code (expected 8 characters like ABCD-EFGH)")
)

// HostPort returns addr with DefaultPort added when it has no port.
func HostPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	if len(addr) > 1 && addr[0] == '[' && addr[len(addr)-1] == ']' {
		addr = addr[1 : len(addr)-1]
	}
	return net.JoinHostPort(addr, strconv.Itoa(DefaultPort))
}

// DialLocal connects to the daemon on the local Unix socket.
func DialLocal(ctx context.Context) (*Client, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", config.SocketPath())
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("connect to local daemon: %w", err)
	}
	conn, hello, err := handshake(ctx, nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if hello.AuthRequired {
		nc.Close()
		return nil, errors.New("local daemon unexpectedly requires authentication")
	}
	return newClient(conn, hello), nil
}

// DialRemote connects to a paired daemon over TLS, pinning its certificate
// to known.ID, and authenticates as dev.
func DialRemote(ctx context.Context, addr string, known identity.KnownServer, dev *identity.Device) (*Client, error) {
	nc, fp, err := dialTLS(ctx, HostPort(addr), known.ID)
	if err != nil {
		return nil, err
	}
	conn, hello, err := handshake(ctx, nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if hello.ServerId != "" && hello.ServerId != known.ID {
		nc.Close()
		return nil, ErrFingerprintMismatch
	}
	c := newClient(conn, hello)
	deviceID := known.DeviceID
	if deviceID == "" {
		deviceID = dev.ID
	}
	_, err = unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Auth{Auth: &fleetv1.AuthRequest{
		DeviceId:  deviceID,
		Signature: dev.Sign(hello.Nonce, fp),
	}}}, (*fleetv1.ServerMessage).GetAuth)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("authenticate with %s: %w", known.Name, err)
	}
	return c, nil
}

// Pair pairs dev with the daemon at addr using a code from `fleet pair`,
// without confirming the server fingerprint; see PairVerify.
func Pair(ctx context.Context, addr, code, deviceName string, dev *identity.Device) (identity.KnownServer, error) {
	return PairVerify(ctx, addr, code, deviceName, dev, nil)
}

// MinFingerprintLen is the fewest hex digits of a server fingerprint a user
// must confirm (64 bits: too many to forge a matching certificate).
const MinFingerprintLen = 16

// ErrFingerprintRejected means the observed server fingerprint does not
// match the one the user expected.
var ErrFingerprintRejected = errors.New("server fingerprint does not match the one shown by `fleet pair` (possible interception); not pairing")

// MatchFingerprint returns a verify func for PairVerify that accepts a server
// whose id starts with want (hex; spaces and case are ignored; at least
// MinFingerprintLen digits).
func MatchFingerprint(want string) (func(serverID string) error, error) {
	want = strings.ToLower(strings.Join(strings.Fields(want), ""))
	if len(want) < MinFingerprintLen {
		return nil, fmt.Errorf("fingerprint must have at least %d hex digits", MinFingerprintLen)
	}
	if _, err := hex.DecodeString(want[:len(want)&^1]); err != nil {
		return nil, fmt.Errorf("fingerprint is not hex: %q", want)
	}
	return func(serverID string) error {
		if !strings.HasPrefix(serverID, want) {
			return ErrFingerprintRejected
		}
		return nil
	}, nil
}

// PairVerify pairs dev with the daemon at addr using a code from `fleet
// pair`. The returned KnownServer should then be stored.
//
// The pairing proof is only sent after verify (if non-nil) accepted the
// observed server id (hex SHA-256 of its certificate). Confirming it against
// the fingerprint `fleet pair` shows is what stops an active attacker on the
// LAN: a proof sent to an impostor lets it brute-force the short code
// offline and pair with the real daemon. The server must also prove it knows
// the code.
func PairVerify(ctx context.Context, addr, code, deviceName string, dev *identity.Device, verify func(serverID string) error) (identity.KnownServer, error) {
	if identity.NormalizeCode(code) == "" {
		return identity.KnownServer{}, ErrInvalidCode
	}
	addr = HostPort(addr)
	nc, fp, err := dialTLS(ctx, addr, "")
	if err != nil {
		return identity.KnownServer{}, err
	}
	conn, hello, err := handshake(ctx, nc)
	if err != nil {
		nc.Close()
		return identity.KnownServer{}, err
	}
	serverID := hex.EncodeToString(fp)
	if hello.ServerId != "" && hello.ServerId != serverID {
		nc.Close()
		return identity.KnownServer{}, ErrFingerprintMismatch
	}
	if verify != nil {
		if err := verify(serverID); err != nil {
			nc.Close()
			return identity.KnownServer{}, err
		}
	}
	c := newClient(conn, hello)
	defer c.Close()
	r, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Pair{Pair: &fleetv1.PairRequest{
		DeviceName:      deviceName,
		DevicePublicKey: dev.Public,
		Proof:           identity.PairProof(code, fp, dev.Public),
		DevicePlatform:  fmt.Sprintf("fleet-cli %s/%s", runtime.GOOS, runtime.GOARCH),
	}}}, (*fleetv1.ServerMessage).GetPair)
	if err != nil {
		return identity.KnownServer{}, fmt.Errorf("pair: %w", err)
	}
	if !hmac.Equal(r.ServerProof, identity.ServerPairProof(code, fp, dev.Public)) {
		return identity.KnownServer{}, ErrServerProof
	}
	name := r.ServerName
	if name == "" {
		name = hello.ServerName
	}
	deviceID := r.DeviceId
	if deviceID == "" {
		deviceID = dev.ID
	}
	return identity.KnownServer{
		ID:       serverID,
		Name:     name,
		Address:  addr,
		DeviceID: deviceID,
		PairedAt: time.Now().UTC(),
	}, nil
}

// dialTLS opens a TLS 1.3 connection. Chain verification is skipped (the
// daemon cert is self-signed); if pin is non-empty the certificate's SHA-256
// must equal it. It returns the observed certificate fingerprint.
func dialTLS(ctx context.Context, addr, pin string) (*tls.Conn, []byte, error) {
	var fp []byte
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by fingerprint pinning / pairing proof
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server sent no certificate")
			}
			got := identity.CertFingerprint(cs.PeerCertificates[0].Raw)
			if pin != "" && subtle.ConstantTimeCompare([]byte(hex.EncodeToString(got)), []byte(pin)) != 1 {
				return ErrFingerprintMismatch
			}
			fp = got
			return nil
		},
	}
	if host, _, err := net.SplitHostPort(addr); err == nil && net.ParseIP(host) == nil {
		cfg.ServerName = host
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	d := tls.Dialer{NetDialer: &net.Dialer{KeepAlive: 30 * time.Second}, Config: cfg}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if errors.Is(err, ErrFingerprintMismatch) {
			return nil, nil, ErrFingerprintMismatch
		}
		return nil, nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	return nc.(*tls.Conn), fp, nil
}
