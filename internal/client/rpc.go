package client

import (
	"context"
	"time"

	fleetv1 "fleet/gen/fleetv1"
)

// Ping round-trips a Ping.
func (c *Client) Ping(ctx context.Context) error {
	_, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Ping{Ping: &fleetv1.Ping{}}},
		(*fleetv1.ServerMessage).GetPong)
	return err
}

// GetInfo describes the daemon.
func (c *Client) GetInfo(ctx context.Context) (*fleetv1.GetInfoResponse, error) {
	return unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_GetInfo{GetInfo: &fleetv1.GetInfoRequest{}}},
		(*fleetv1.ServerMessage).GetGetInfo)
}

// ListAdapters lists the daemon's adapters.
func (c *Client) ListAdapters(ctx context.Context) ([]*fleetv1.AdapterInfo, error) {
	r, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_ListAdapters{ListAdapters: &fleetv1.ListAdaptersRequest{}}},
		(*fleetv1.ServerMessage).GetListAdapters)
	return r.GetAdapters(), err
}

// ListRoots lists the configured roots.
func (c *Client) ListRoots(ctx context.Context) ([]*fleetv1.Root, error) {
	r, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_ListRoots{ListRoots: &fleetv1.ListRootsRequest{}}},
		(*fleetv1.ServerMessage).GetListRoots)
	return r.GetRoots(), err
}

// AddRoot adds a root. The path must be absolute on the daemon host.
func (c *Client) AddRoot(ctx context.Context, in *fleetv1.AddRootRequest) (*fleetv1.Root, error) {
	r, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_AddRoot{AddRoot: in}},
		(*fleetv1.ServerMessage).GetAddRoot)
	return r.GetRoot(), err
}

// RemoveRoot removes a root by name.
func (c *Client) RemoveRoot(ctx context.Context, name string) error {
	_, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_RemoveRoot{RemoveRoot: &fleetv1.RemoveRootRequest{Name: name}}},
		(*fleetv1.ServerMessage).GetRemoveRoot)
	return err
}

// Browse lists sub-directories inside a root.
func (c *Client) Browse(ctx context.Context, in *fleetv1.BrowseRequest) (*fleetv1.BrowseResponse, error) {
	return unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Browse{Browse: in}},
		(*fleetv1.ServerMessage).GetBrowse)
}

// ListAgents lists live agents, plus finished ones if includeFinished.
func (c *Client) ListAgents(ctx context.Context, includeFinished bool) ([]*fleetv1.Agent, error) {
	r, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_ListAgents{ListAgents: &fleetv1.ListAgentsRequest{IncludeFinished: includeFinished}}},
		(*fleetv1.ServerMessage).GetListAgents)
	return r.GetAgents(), err
}

// RunAgent starts an agent.
func (c *Client) RunAgent(ctx context.Context, in *fleetv1.RunAgentRequest) (*fleetv1.Agent, error) {
	r, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_RunAgent{RunAgent: in}},
		(*fleetv1.ServerMessage).GetRunAgent)
	return r.GetAgent(), err
}

// KillAgent stops (and optionally forgets) an agent.
func (c *Client) KillAgent(ctx context.Context, in *fleetv1.KillAgentRequest) (*fleetv1.KillAgentResponse, error) {
	return unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_KillAgent{KillAgent: in}},
		(*fleetv1.ServerMessage).GetKillAgent)
}

// SendText types text into an agent's terminal, pressing Enter if submit.
func (c *Client) SendText(ctx context.Context, agent, text string, submit bool) error {
	_, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_SendText{SendText: &fleetv1.SendTextRequest{Agent: agent, Text: text, Submit: submit}}},
		(*fleetv1.ServerMessage).GetSendText)
	return err
}

// CreatePairingCode issues a one-time pairing code. ttl <= 0 uses the
// daemon default.
func (c *Client) CreatePairingCode(ctx context.Context, ttl time.Duration) (*fleetv1.CreatePairingCodeResponse, error) {
	var secs uint32
	if ttl > 0 {
		secs = uint32((ttl + time.Second - 1) / time.Second)
	}
	return unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_CreatePairingCode{CreatePairingCode: &fleetv1.CreatePairingCodeRequest{TtlSeconds: secs}}},
		(*fleetv1.ServerMessage).GetCreatePairingCode)
}

// ListDevices lists paired devices.
func (c *Client) ListDevices(ctx context.Context) ([]*fleetv1.Device, error) {
	r, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_ListDevices{ListDevices: &fleetv1.ListDevicesRequest{}}},
		(*fleetv1.ServerMessage).GetListDevices)
	return r.GetDevices(), err
}

// RevokeDevice unpairs a device by id or name.
func (c *Client) RevokeDevice(ctx context.Context, device string) error {
	_, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_RevokeDevice{RevokeDevice: &fleetv1.RevokeDeviceRequest{Device: device}}},
		(*fleetv1.ServerMessage).GetRevokeDevice)
	return err
}

// Hook delivers an agent hook event (local socket only). With ev.Wait set
// it may wait for the user's answer to what the hook asks.
func (c *Client) Hook(ctx context.Context, ev *fleetv1.HookEvent) (*fleetv1.HookResponse, error) {
	return unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Hook{Hook: ev}},
		(*fleetv1.ServerMessage).GetHook)
}

// Subscribe starts the event stream. The daemon first sends one
// AgentUpserted per agent, then SnapshotDone, then live changes. The channel
// is closed when ctx is done or the connection ends. Keep reading it: a full
// channel stalls the connection.
func (c *Client) Subscribe(ctx context.Context) (<-chan *fleetv1.Event, error) {
	s := newStream[*fleetv1.Event](256)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.subs[s] = struct{}{}
	c.mu.Unlock()
	unsubscribe := func() {
		c.mu.Lock()
		delete(c.subs, s)
		c.mu.Unlock()
		s.close()
	}
	if _, err := unary(ctx, c, &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Subscribe{Subscribe: &fleetv1.SubscribeRequest{}}},
		(*fleetv1.ServerMessage).GetSubscribe); err != nil {
		unsubscribe()
		return nil, err
	}
	context.AfterFunc(ctx, unsubscribe)
	return s.ch, nil
}
