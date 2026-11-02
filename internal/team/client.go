package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxReply caps a hub reply (a bundle's data is the largest).
const maxReply = MaxBundleData + 4096

// HubError is a hub reply with an error status.
type HubError struct {
	Status  int
	Message string
}

func (e *HubError) Error() string {
	if e.Message == "" {
		return "hub: status " + strconv.Itoa(e.Status)
	}
	return "hub: " + e.Message
}

// IsStatus reports whether err is a HubError with status.
func IsStatus(err error, status int) bool {
	var he *HubError
	return errors.As(err, &he) && he.Status == status
}

// Client talks to a hub as one member of one team: every request is signed
// with Key. Create and Join are the two calls made before one is a member.
type Client struct {
	Key *MemberKey
	// HTTP is the client used; NewClient sets one with no proxy from the
	// environment and a timeout long enough for an events long poll.
	HTTP *http.Client
}

// NewClient returns a client for key's team on key's hub.
func NewClient(key *MemberKey) *Client {
	return &Client{Key: key, HTTP: newHTTPClient()}
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   90 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConnsPerHost: 2, IdleConnTimeout: 60 * time.Second},
	}
}

// Create registers a new team on hub: the founding roster (Found) and the
// founder's sealed key. code is the operator's creation code ("" on an
// open hub).
func Create(ctx context.Context, hub, code string, req CreateRequest) error {
	hub = strings.TrimRight(hub, "/")
	auth := ""
	if code != "" {
		auth = CreateScheme + code
	}
	_, err := do(ctx, newHTTPClient(), http.MethodPost, hub+"/v1/teams", auth, jsonBody(req), jsonType, nil)
	return err
}

// Join sends a join request with the invite's credential.
func Join(ctx context.Context, inv *Invite, req JoinRequest) error {
	_, err := do(ctx, newHTTPClient(), http.MethodPost, inv.Hub+"/v1/teams/"+inv.Team+"/joins", InviteAuthScheme+inv.Credential(), jsonBody(req), jsonType, nil)
	return err
}

// Rosters returns the rosters above seq since (0 for all).
func (c *Client) Rosters(ctx context.Context, since uint64) ([]*Roster, error) {
	var out RosterList
	err := c.call(ctx, http.MethodGet, "/roster?since="+strconv.FormatUint(since, 10), nil, &out)
	return out.Rosters, err
}

// PostRoster publishes the next roster (admins), with the sealed keys it
// needs.
func (c *Client) PostRoster(ctx context.Context, r *Roster, keys []SealedKey) error {
	return c.call(ctx, http.MethodPost, "/roster", jsonBody(RosterPost{Roster: r, Keys: keys}), nil)
}

// Keys returns the team keys sealed to this member.
func (c *Client) Keys(ctx context.Context) ([]SealedKey, error) {
	var out KeyList
	err := c.call(ctx, http.MethodGet, "/keys", nil, &out)
	return out.Keys, err
}

// PostKeys stores sealed keys for members (admins).
func (c *Client) PostKeys(ctx context.Context, keys []SealedKey) error {
	return c.call(ctx, http.MethodPost, "/keys", jsonBody(KeyList{Keys: keys}), nil)
}

// RegisterInvite registers inv on the hub (admins), good until expires.
func (c *Client) RegisterInvite(ctx context.Context, inv *Invite, expires time.Time) error {
	return c.call(ctx, http.MethodPost, "/invites", jsonBody(InviteRegistration{ID: inv.ID(), Expires: expires}), nil)
}

// Joins lists pending join requests and members who left (admins).
func (c *Client) Joins(ctx context.Context) (JoinList, error) {
	var out JoinList
	err := c.call(ctx, http.MethodGet, "/joins", nil, &out)
	return out, err
}

// DeleteJoin removes a join request once handled (admins).
func (c *Client) DeleteJoin(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "/joins/"+id, nil, nil)
}

// Leave leaves the team: the hub stops taking this member's requests at
// once and tells the admins, who remove the member from the roster.
func (c *Client) Leave(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/leave", nil, nil)
}

// Heads lists the current head of every bundle.
func (c *Client) Heads(ctx context.Context) ([]*Head, error) {
	var out HeadList
	err := c.call(ctx, http.MethodGet, "/bundles", nil, &out)
	return out.Heads, err
}

// Head returns the current head of bundle id.
func (c *Client) Head(ctx context.Context, id string) (*Head, error) {
	var out Head
	err := c.call(ctx, http.MethodGet, "/bundles/"+id, nil, &out)
	return &out, err
}

// PutHead publishes a new version of an own bundle (after PutData for its
// data, unless it is a re-wrap or a tombstone).
func (c *Client) PutHead(ctx context.Context, h *Head) error {
	return c.call(ctx, http.MethodPut, "/bundles/"+h.ID, jsonBody(h), nil)
}

// PutData uploads sealed bundle data; ref must be DataRef(data).
func (c *Client) PutData(ctx context.Context, ref string, data []byte) error {
	_, err := c.raw(ctx, http.MethodPut, "/data/"+ref, data, "application/octet-stream")
	return err
}

// Data downloads sealed bundle data. The caller checks it with VerifyData.
func (c *Client) Data(ctx context.Context, ref string) ([]byte, error) {
	return c.raw(ctx, http.MethodGet, "/data/"+ref, nil, "")
}

// Events long-polls the team's events after cursor since (0 for the
// start). It returns after at most about 25 seconds, with no events if
// nothing happened.
func (c *Client) Events(ctx context.Context, since uint64) (EventList, error) {
	var out EventList
	err := c.call(ctx, http.MethodGet, "/events?since="+strconv.FormatUint(since, 10), nil, &out)
	return out, err
}

// call sends a signed request to a team route and decodes a JSON reply
// into out (nil to ignore).
func (c *Client) call(ctx context.Context, method, route string, body []byte, out any) error {
	reply, err := c.raw(ctx, method, route, body, jsonType)
	if err != nil || out == nil {
		return err
	}
	if err := json.Unmarshal(reply, out); err != nil {
		return fmt.Errorf("hub: bad reply: %w", err)
	}
	return nil
}

// jsonType is the Content-Type of JSON bodies.
const jsonType = "application/json"

// raw sends a signed request and returns the reply body.
func (c *Client) raw(ctx context.Context, method, route string, body []byte, ctype string) ([]byte, error) {
	base := strings.TrimRight(c.Key.Hub, "/")
	nonce, err := c.nonce(ctx, base)
	if err != nil {
		return nil, err
	}
	uri := "/v1/teams/" + c.Key.Team + route
	auth := c.Key.SignRequest(nonce, method, uri, body)
	return do(ctx, c.HTTP, method, base+uri, auth, body, ctype, nil)
}

func (c *Client) nonce(ctx context.Context, base string) (string, error) {
	var v NonceReply
	if _, err := do(ctx, c.HTTP, http.MethodGet, base+"/v1/nonce", "", nil, "", &v); err != nil {
		return "", err
	}
	if v.Nonce == "" {
		return "", errors.New("hub: no nonce")
	}
	return v.Nonce, nil
}

// do sends one request. A non-2xx status becomes a HubError with the hub's
// message. out, if given, receives the decoded JSON reply.
func do(ctx context.Context, hc *http.Client, method, url, auth string, body []byte, ctype string, out any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", ctype)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return nil, err
	}
	if len(reply) > maxReply {
		return nil, errors.New("hub: reply too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		he := &HubError{Status: resp.StatusCode}
		var e Error
		if json.Unmarshal(reply, &e) == nil {
			he.Message = e.Error
		}
		return nil, he
	}
	if out != nil {
		if err := json.Unmarshal(reply, out); err != nil {
			return nil, fmt.Errorf("hub: bad reply: %w", err)
		}
	}
	return reply, nil
}

// jsonBody marshals v; the types here always marshal.
func jsonBody(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("team: marshal: " + err.Error())
	}
	return b
}
