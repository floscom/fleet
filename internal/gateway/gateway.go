// Package gateway is the daemon side of fleet teams (docs/TEAMS.md): it
// holds this fleet's membership in each team it created or joined, talks
// to the teams' hubs, and moves skills between the team and the skill
// store (package skills).
//
// A Gateway manages every team of one daemon. A team's state lives under
// <dir>/<team id>/: the member key (member-key.json), and state.json with
// the trusted roster head, the team keys this member holds, the secrets of
// its open invites, what it publishes, what it subscribes to and pins, the
// highest version seen of every bundle, and the event cursor. Updates
// waiting behind a pin are kept decrypted in pending/<bundle>.tar.gz.
// Everything is written atomically with mode 0600: the state holds team
// keys and invite secrets.
//
// Each team has a loop (run.go). It long-polls the hub's events and acts
// on them, and does a full sync (rosters, keys, joins, heads) on start,
// when the hub says its log was reset, after an error, and every
// fullSyncEvery. The hub is never trusted as such: rosters advance only
// through team.Advance from the trusted head (a joiner first anchors with
// team.Anchor), keys open only against the roster's key check, heads are
// checked with team.VerifyHead against the trusted roster and their data
// with team.VerifyData, and a bundle's version never goes back. What fails
// verification is logged and skipped. While the hub is unreachable the
// loop backs off and keeps the last state.
//
// The member's duties:
//
//   - an admin adds whoever joins with one of its own invites (the invite
//     is the approval: Invite.VerifyJoin, team.AddMember, the joiner's
//     sealed key, one roster post), and drops members who left with a new
//     epoch (team.RemoveMember, every member's sealed key). When admins
//     race, the hub's conflict makes the loser fetch the chain and try
//     again.
//   - an owner publishes the skills it was asked to (own ones only, never
//     a team skill), sends a new version whenever the store's hash of one
//     differs from the last published once the store has the skill settled,
//     withdraws one (team.Tombstone) when it is unpublished or deleted, and
//     re-wraps its live heads after an epoch change (team.Rewrap).
//   - a subscriber installs the bundles that match its subscriptions
//     ("<owner>/<name>" or "<owner>/*") into the store, updates them, keeps
//     a pinned one's newer version pending until applied, removes one its
//     owner withdrew, and leaves alone one that was forked. A team skill the
//     user deleted is unsubscribed: it is not installed again until it is
//     subscribed to by name. Installed skills whose publisher left stay.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"fleet/internal/skills"
	"fleet/internal/team"
)

// Kind classifies an *Error for the caller (the web API maps it to a
// status).
type Kind int

const (
	Invalid   Kind = iota + 1 // the request makes no sense
	NotFound                  // no such team, member, skill or bundle
	Forbidden                 // not an admin, not the owner, not one's own skill
	Conflict                  // not possible in the current state
	Upstream                  // the hub refused or could not be reached
)

// Error is an error meant for the user.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func errorf(kind Kind, format string, args ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// hubErr makes the error of a hub call one for the user: the hub's
// refusal, or that it could not be reached. Anything else (a local
// failure) is returned as it is.
func hubErr(err error) error {
	var he *team.HubError
	var ue *url.Error
	var ge *Error
	switch {
	case errors.As(err, &ge):
		return err
	case errors.As(err, &he) && he.Status == http.StatusConflict:
		return &Error{Kind: Conflict, Msg: he.Error()}
	case errors.As(err, &he):
		return &Error{Kind: Upstream, Msg: he.Error()}
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Kind: Upstream, Msg: "the hub did not answer in time"}
	case errors.As(err, &ue):
		return &Error{Kind: Upstream, Msg: "the hub could not be reached: " + ue.Err.Error()}
	case errors.Is(err, team.ErrVerify):
		return &Error{Kind: Upstream, Msg: "the hub sent something that does not verify: " + err.Error()}
	}
	return err
}

// ParseTTL parses an invite's lifetime: a Go duration ("72h") or days
// ("7d"); "" is 0, the default.
func ParseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n <= 0 {
			return 0, errorf(Invalid, "bad invite lifetime %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, errorf(Invalid, "bad invite lifetime %q: say 7d or 48h", s)
	}
	return d, nil
}

// Options configures Open.
type Options struct {
	// Dir holds a folder per team (<FLEET_HOME>/teams).
	Dir string
	// Skills is the store team skills are published from and installed
	// into.
	Skills *skills.Store
	// HTTP is the client for hub requests; nil uses team.NewClient's.
	HTTP *http.Client
	Log  *slog.Logger
}

// Gateway is this daemon's membership in its teams.
type Gateway struct {
	opts Options
	log  *slog.Logger

	mu    sync.Mutex
	teams map[string]*teamRT
	// ctx is Run's context, nil before Run: teams created or joined later
	// start their loops with it.
	ctx context.Context
	wg  sync.WaitGroup
}

// Open loads the teams under opts.Dir.
func Open(opts Options) (*Gateway, error) {
	if opts.Skills == nil {
		return nil, errors.New("gateway: a skill store is needed")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}
	g := &Gateway{opts: opts, log: opts.Log, teams: map[string]*teamRT{}}
	des, err := os.ReadDir(opts.Dir)
	if err != nil {
		return nil, err
	}
	for _, de := range des {
		if !de.IsDir() || !team.ValidID(de.Name()) {
			continue
		}
		t, err := g.load(filepath.Join(opts.Dir, de.Name()))
		if err != nil {
			// One broken team must not take the others with it.
			g.log.Warn("team: loading failed", "team", de.Name(), "err", err)
			continue
		}
		g.teams[t.id] = t
	}
	return g, nil
}

// Run runs every team's loop, and that of every team created or joined
// meanwhile, until ctx ends.
func (g *Gateway) Run(ctx context.Context) {
	g.mu.Lock()
	g.ctx = ctx
	for _, t := range g.teams {
		g.startLocked(t)
	}
	g.mu.Unlock()
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.watchStore(ctx)
	}()
	<-ctx.Done()
	g.wg.Wait()
}

// startLocked starts t's loop if Run is running.
func (g *Gateway) startLocked(t *teamRT) {
	if g.ctx == nil || g.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(g.ctx)
	t.cancel = cancel
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		t.run(ctx)
	}()
}

// watchStore tells every team when the skill store changed. The store's
// Changed channel has one reader already (the fleet sync, web/skills.go),
// so the manifest is compared instead; it is in memory, and a change is
// only taken once it held still for a scan anyway.
func (g *Gateway) watchStore(ctx context.Context) {
	t := time.NewTicker(storePoll)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if d := manifestDigest(g.opts.Skills.Manifest()); d != last {
			last = d
			for _, tm := range g.list() {
				tm.poke(tm.storeKick)
			}
		}
	}
}

// manifestDigest changes whenever an entry of the manifest does.
func manifestDigest(m []skills.Entry) string {
	var b strings.Builder
	for _, e := range m {
		fmt.Fprintf(&b, "%s %s %d %s %t\n", e.Name, e.Hash, e.Version, e.By, e.Deleted)
	}
	return b.String()
}

// list returns the teams, by id.
func (g *Gateway) list() []*teamRT {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*teamRT, 0, len(g.teams))
	for _, t := range g.teams {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *teamRT) int { return strings.Compare(a.id, b.id) })
	return out
}

// find picks a team: by id, by name (any case) or by id prefix; "" is the
// only one.
func (g *Gateway) find(ref string) (*teamRT, error) {
	all := g.list()
	ref = strings.TrimSpace(ref)
	if ref == "" {
		switch len(all) {
		case 0:
			return nil, errorf(NotFound, "this fleet is in no team: create one or join one")
		case 1:
			return all[0], nil
		}
		return nil, errorf(Invalid, "this fleet is in %d teams: say which one", len(all))
	}
	var byName, byPrefix []*teamRT
	for _, t := range all {
		if t.id == ref {
			return t, nil
		}
		if strings.EqualFold(t.name(), ref) {
			byName = append(byName, t)
		}
		if strings.HasPrefix(t.id, strings.ToLower(ref)) {
			byPrefix = append(byPrefix, t)
		}
	}
	for _, c := range [][]*teamRT{byName, byPrefix} {
		switch len(c) {
		case 0:
			continue
		case 1:
			return c[0], nil
		}
		return nil, errorf(Invalid, "%q names %d teams: use the team id", ref, len(c))
	}
	return nil, errorf(NotFound, "no team %q", ref)
}

// checkName checks a display name (of a member or a team).
func checkName(what, s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errorf(Invalid, "a %s name is needed", what)
	}
	if len(s) > maxNameLen {
		return "", errorf(Invalid, "a %s name is at most %d bytes", what, maxNameLen)
	}
	if strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == '/' }) {
		return "", errorf(Invalid, "a %s name cannot hold / or control characters", what)
	}
	return s, nil
}

// cleanName makes a joiner's name one checkName takes, falling back to
// the start of its id: the joiner chose it, and refusing the join for it
// would leave them waiting with no word why.
func cleanName(s, id string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/':
			return '-'
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	for len(s) > maxNameLen {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	if s = strings.TrimSpace(s); s == "" {
		return id[:8]
	}
	return s
}

// checkHub normalizes a hub URL.
func checkHub(hub string) (string, error) {
	hub = strings.TrimRight(strings.TrimSpace(hub), "/")
	if !strings.HasPrefix(hub, "https://") && !strings.HasPrefix(hub, "http://") {
		return "", errorf(Invalid, "the hub must be an http(s) URL, like https://hub.example.com")
	}
	return hub, nil
}

// Create founds a team called name on hub, with this fleet as its first
// member and admin, called me. code is the hub operator's creation code
// ("" for an open hub).
func (g *Gateway) Create(ctx context.Context, hub, code, name, me string) (TeamView, error) {
	hub, err := checkHub(hub)
	if err != nil {
		return TeamView{}, err
	}
	if name, err = checkName("team", name); err != nil {
		return TeamView{}, err
	}
	if me, err = checkName("member", me); err != nil {
		return TeamView{}, err
	}
	id := team.NewID()
	key, err := team.NewMemberKey(hub, id)
	if err != nil {
		return TeamView{}, err
	}
	r, K, err := team.Found(key, name, me)
	if err != nil {
		return TeamView{}, err
	}
	keys, err := team.SealKeys(r, K)
	if err != nil {
		return TeamView{}, err
	}
	if err := team.Create(ctx, hub, strings.TrimSpace(code), team.CreateRequest{Roster: r, Keys: keys}); err != nil {
		if team.IsStatus(err, http.StatusUnauthorized) {
			return TeamView{}, errorf(Forbidden, "the hub wants a creation code: ask its operator for one (fleet hub code)")
		}
		return TeamView{}, hubErr(err)
	}
	st := newState(hub, id, me)
	st.Roster = r
	st.addKey(r.Epoch, K)
	t, err := g.add(key, st)
	if err != nil {
		return TeamView{}, err
	}
	g.log.Info("team: created", "team", id, "hub", hub)
	return t.view(), nil
}

// Join joins the team of an invite, as me. The fleet is a member once an
// admin's gateway has added it, which it does by itself when it next
// talks to the hub; until then the team waits.
func (g *Gateway) Join(ctx context.Context, invite, me string) (TeamView, error) {
	inv, err := team.ParseInvite(invite)
	if err != nil {
		return TeamView{}, errorf(Invalid, "%s", strings.TrimPrefix(err.Error(), "team: "))
	}
	if me, err = checkName("member", me); err != nil {
		return TeamView{}, err
	}
	if t, err := g.find(inv.Team); err == nil && t.id == inv.Team {
		return TeamView{}, errorf(Conflict, "this fleet is in that team already")
	}
	key, err := team.NewMemberKey(inv.Hub, inv.Team)
	if err != nil {
		return TeamView{}, err
	}
	req, err := inv.NewJoinRequest(key, me)
	if err != nil {
		return TeamView{}, err
	}
	if err := team.Join(ctx, inv, req); err != nil {
		if team.IsStatus(err, http.StatusForbidden) {
			return TeamView{}, errorf(Forbidden, "the hub does not know that invite, or it was used or has expired: ask for a new one")
		}
		return TeamView{}, hubErr(err)
	}
	st := newState(inv.Hub, inv.Team, me)
	st.Inviter = inv.Admin
	t, err := g.add(key, st)
	if err != nil {
		return TeamView{}, err
	}
	g.log.Info("team: join requested", "team", inv.Team, "hub", inv.Hub)
	return t.view(), nil
}

// add saves a new team and starts it.
func (g *Gateway) add(key *team.MemberKey, st *state) (*teamRT, error) {
	dir := filepath.Join(g.opts.Dir, key.Team)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := key.Save(filepath.Join(dir, keyFile)); err != nil {
		return nil, err
	}
	t := g.newTeam(dir, key, st)
	if err := t.save(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.teams[t.id] = t
	g.startLocked(t)
	g.mu.Unlock()
	t.poke(t.kick)
	return t, nil
}

// Teams describes every team.
func (g *Gateway) Teams() []TeamView {
	all := g.list()
	out := make([]TeamView, 0, len(all))
	for _, t := range all {
		out = append(out, t.view())
	}
	return out
}

// Team describes one team (see find for ref).
func (g *Gateway) Team(ref string) (TeamView, error) {
	t, err := g.find(ref)
	if err != nil {
		return TeamView{}, err
	}
	return t.view(), nil
}

// Sync syncs one team (ref as for find) or, with ref "", every team now,
// and returns the first error.
func (g *Gateway) Sync(ctx context.Context, ref string) error {
	teams := g.list()
	if ref != "" {
		t, err := g.find(ref)
		if err != nil {
			return err
		}
		teams = []*teamRT{t}
	}
	var first error
	for _, t := range teams {
		if err := t.fullSync(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// forget stops t and deletes its state, after leaving.
func (g *Gateway) forget(t *teamRT) {
	g.mu.Lock()
	delete(g.teams, t.id)
	g.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
	if err := os.RemoveAll(t.dir); err != nil {
		g.log.Warn("team: removing its state failed", "team", t.id, "err", err)
	}
}
