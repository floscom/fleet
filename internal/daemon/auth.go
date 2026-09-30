package daemon

import (
	"context"
	"crypto/ed25519"
	"strings"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/identity"
)

const (
	defaultCodeTTL = 5 * time.Minute
	// maxCodeTTL keeps the window for an offline attack on an intercepted
	// pairing proof short (see identity.PairingCodes.Issue).
	maxCodeTTL = 5 * time.Minute
	maxNameLen = 64
)

// handlePair redeems a pairing code. On success the device is stored and the
// connection is authenticated.
func (c *conn) handlePair(id uint64, req *fleetv1.PairRequest) error {
	if c.local {
		return errf(codeUnauth, "pairing is only possible over TLS")
	}
	pub := req.GetDevicePublicKey()
	if len(pub) != ed25519.PublicKeySize {
		return errf(codeInvalid, "device_public_key must be %d bytes", ed25519.PublicKeySize)
	}
	d := c.d
	code, ok := d.codes.Redeem(req.GetProof(), d.server.Fingerprint, pub)
	if !ok {
		d.log.Warn("pairing failed", "remote", c.raw.RemoteAddr().String())
		if c.fail() {
			c.sendError(id, codePairing, "pairing failed")
			return errClose
		}
		return errf(codePairing, "pairing code wrong, expired or exhausted")
	}
	name := clean(req.GetDeviceName(), maxNameLen)
	if name == "" {
		name = "device"
	}
	now := time.Now()
	dev := identity.PairedDevice{
		ID:        identity.DeviceID(pub),
		Name:      name,
		Platform:  clean(req.GetDevicePlatform(), maxNameLen),
		PublicKey: pub,
		PairedAt:  now,
		LastSeen:  now,
	}
	if err := d.devices.Add(dev); err != nil {
		return err
	}
	if !c.authenticate(id, dev.ID) {
		return errClose
	}
	d.log.Info("device paired", "device", dev.ID, "name", dev.Name, "platform", dev.Platform)
	return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Pair{Pair: &fleetv1.PairResponse{
		DeviceId:    dev.ID,
		ServerProof: identity.ServerPairProof(code, d.server.Fingerprint, pub),
		ServerId:    d.server.ID,
		ServerName:  d.config().Name,
	}}})
}

// handleAuth verifies a paired device's signature over the hello nonce.
func (c *conn) handleAuth(id uint64, req *fleetv1.AuthRequest) error {
	if c.local {
		return errf(codeUnauth, "authentication is not needed on the local socket")
	}
	d := c.d
	dev, ok := d.devices.Get(req.GetDeviceId())
	if !ok {
		d.log.Warn("auth by unknown device", "device", req.GetDeviceId(), "remote", c.raw.RemoteAddr().String())
		c.fail()
		c.sendError(id, codeUnauth, "unknown or revoked device")
		return errClose
	}
	if !identity.VerifyAuth(dev.PublicKey, c.nonce, d.server.Fingerprint, req.GetSignature()) {
		d.log.Warn("auth signature invalid", "device", dev.ID)
		if c.fail() {
			c.sendError(id, codeUnauth, "authentication failed")
			return errClose
		}
		return errf(codeUnauth, "authentication failed")
	}
	if !c.authenticate(id, dev.ID) {
		return errClose
	}
	d.devices.Touch(dev.ID, time.Now())
	d.log.Debug("device authenticated", "device", dev.ID, "name", dev.Name)
	return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_Auth{Auth: &fleetv1.AuthResponse{DeviceName: dev.Name}}})
}

// authenticate marks the connection as deviceID's, then re-checks that the
// device still exists: a revoke that ran between the caller's lookup and
// setAuthed did not see this connection, so it is refused here instead
// (with an Error reply to request id). It reports whether c may continue.
func (c *conn) authenticate(id uint64, deviceID string) bool {
	c.setAuthed(deviceID)
	if _, ok := c.d.devices.Get(deviceID); !ok {
		c.sendError(id, codeUnauth, "unknown or revoked device")
		return false
	}
	return true
}

func (c *conn) handleCreatePairingCode(id uint64, req *fleetv1.CreatePairingCodeRequest) error {
	ttl := time.Duration(req.GetTtlSeconds()) * time.Second
	if ttl <= 0 {
		ttl = defaultCodeTTL
	}
	ttl = min(ttl, maxCodeTTL)
	code, expires := c.d.codes.Issue(ttl)
	c.d.log.Info("pairing code issued", "expires", expires.Format(time.TimeOnly))
	return c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_CreatePairingCode{
		CreatePairingCode: &fleetv1.CreatePairingCodeResponse{
			Code:        code,
			ExpiresAtMs: expires.UnixMilli(),
			ServerId:    c.d.server.ID,
		},
	}})
}

// handleRevoke removes a device and closes its live connections (after
// replying, in case the caller revoked itself).
func (c *conn) handleRevoke(id uint64, req *fleetv1.RevokeDeviceRequest) error {
	d := c.d
	dev, ok := d.devices.Find(req.GetDevice())
	if !ok {
		return errf(codeNotFound, "no device %q", req.GetDevice())
	}
	if err := d.devices.Remove(dev.ID); err != nil {
		return err
	}
	d.log.Info("device revoked", "device", dev.ID, "name", dev.Name)
	err := c.reply(id, &fleetv1.ServerMessage{Msg: &fleetv1.ServerMessage_RevokeDevice{RevokeDevice: &fleetv1.RevokeDeviceResponse{}}})
	for _, cc := range d.connsFor(dev.ID) {
		cc.close()
	}
	return err
}

func (d *daemon) listDevices() []*fleetv1.Device {
	var out []*fleetv1.Device
	for _, dev := range d.devices.List() {
		out = append(out, &fleetv1.Device{
			Id:           dev.ID,
			Name:         dev.Name,
			Platform:     dev.Platform,
			PairedAtMs:   dev.PairedAt.UnixMilli(),
			LastSeenAtMs: dev.LastSeen.UnixMilli(),
			Connected:    len(d.connsFor(dev.ID)) > 0,
		})
	}
	return out
}

func (d *daemon) listAdapters(ctx context.Context) *fleetv1.ListAdaptersResponse {
	resp := &fleetv1.ListAdaptersResponse{}
	for _, a := range d.opts.Adapters.All() {
		det := a.Detect(ctx)
		info := &fleetv1.AdapterInfo{
			Id:                a.ID(),
			DisplayName:       a.DisplayName(),
			Available:         det.Available,
			BinaryPath:        det.Path,
			Version:           det.Version,
			UnavailableReason: det.Reason,
			Capabilities:      a.Capabilities().Proto(),
		}
		if md, ok := a.(adapter.Modeler); ok {
			ms := md.Models()
			for _, m := range ms.Models {
				info.Models = append(info.Models, &fleetv1.ModelChoice{Id: m.ID, Label: m.Label, Efforts: m.Efforts})
			}
			for _, e := range ms.Efforts {
				info.Efforts = append(info.Efforts, &fleetv1.EffortChoice{Id: e.ID, Label: e.Label})
			}
		}
		resp.Adapters = append(resp.Adapters, info)
	}
	return resp
}

// clean trims s, drops control characters and caps it at n bytes.
func clean(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "")
	}
	return s
}
