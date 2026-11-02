package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"fleet/internal/team"
)

const (
	keyFile    = "member-key.json"
	stateFile  = "state.json"
	pendingDir = "pending"
	// maxNameLen bounds display names.
	maxNameLen = 64
)

// state is a team's state.json.
type state struct {
	V    int    `json:"v"`
	Hub  string `json:"hub"`
	Team string `json:"team"`
	// Me is this member's display name, as given when creating or
	// joining (the roster has it sealed).
	Me string `json:"me"`
	// Inviter is the admin whose invite this member joined with: the
	// signer of its trust anchor. Empty for the founder.
	Inviter string `json:"inviter,omitempty"`
	// Roster is the trusted roster head; nil until a roster lists this
	// member (a joiner waiting for an admin).
	Roster *team.Roster `json:"roster,omitempty"`
	// Keys are the team keys this member holds, by epoch: the current one,
	// and older ones its own heads (to re-wrap) or others' heads not
	// re-wrapped yet may still be under.
	Keys []epochKey `json:"keys,omitempty"`
	// Cursor is the event cursor.
	Cursor uint64 `json:"cursor"`
	// Removed is set when the hub refuses this member's requests after it
	// was one: an admin removed it.
	Removed bool `json:"removed,omitempty"`
	// Invites are the open invites this member made, with their secrets,
	// kept until used or expired.
	Invites []invite `json:"invites,omitempty"`
	// Published are the own skills published to the team, by skill name.
	Published map[string]*published `json:"published,omitempty"`
	// Subs are the subscriptions.
	Subs []sub `json:"subscriptions,omitempty"`
	// Bundles is what is known of every bundle seen, by id.
	Bundles map[string]*bundle `json:"bundles,omitempty"`
}

// epochKey is the team key of one epoch.
type epochKey struct {
	Epoch uint64 `json:"epoch"`
	Key   []byte `json:"key"`
}

// invite is an open invite's secret, for verifying the join it brings.
type invite struct {
	ID      string    `json:"id"`
	Secret  []byte    `json:"secret"`
	Expires time.Time `json:"expires"`
}

// published is an own skill published to the team.
type published struct {
	Bundle string `json:"bundle"`
	// Hash is the store hash of the files last published; Head is the
	// head that published them (or re-wrapped them), nil before the first
	// upload went through.
	Hash string     `json:"hash,omitempty"`
	Head *team.Head `json:"head,omitempty"`
	// Error is why the last attempt failed; "" when published.
	Error string `json:"error,omitempty"`
	// waiting is set while the store has the skill changing: it goes once
	// it holds still.
	waiting bool
}

// sub is a subscription: the skills called Name ("*" for all) that the
// member Owner (an id) publishes.
type sub struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// bundle is what is known of one bundle.
type bundle struct {
	Owner string `json:"owner"`
	// High is the highest version seen: a lower one is a rollback and
	// refused.
	High int64 `json:"high"`
	// Head is the latest verified head, Meta its opened metadata (nil
	// while it cannot be opened: Problem says why).
	Head    *team.Head `json:"head,omitempty"`
	Meta    *team.Meta `json:"meta,omitempty"`
	Problem string     `json:"problem,omitempty"`
	// Dropped means the user does not want it: unsubscribed, or deleted
	// from the store. A subscription by name takes it back.
	Dropped bool `json:"dropped,omitempty"`
	// TombAck is the store version of the last tombstone of this bundle's
	// skill that was taken into account, so an old tombstone does not
	// drop it again.
	TombAck int64 `json:"tomb_ack,omitempty"`
	Pinned  bool  `json:"pinned,omitempty"`
	// Pending is the newer version a pin holds back (its files are in
	// pending/<id>.tar.gz).
	Pending *pending `json:"pending,omitempty"`
}

// pending is an update held back by a pin.
type pending struct {
	Version int64     `json:"version"`
	Meta    team.Meta `json:"meta"`
}

func newState(hub, id, me string) *state {
	return &state{V: 1, Hub: hub, Team: id, Me: me, Published: map[string]*published{}, Bundles: map[string]*bundle{}}
}

// key returns the team key of epoch.
func (st *state) key(epoch uint64) []byte {
	for _, k := range st.Keys {
		if k.Epoch == epoch {
			return k.Key
		}
	}
	return nil
}

// current is the team key of the roster's epoch.
func (st *state) current() []byte {
	if st.Roster == nil {
		return nil
	}
	return st.key(st.Roster.Epoch)
}

// addKey records the team key of epoch.
func (st *state) addKey(epoch uint64, K []byte) {
	if st.key(epoch) == nil {
		st.Keys = append(st.Keys, epochKey{Epoch: epoch, Key: K})
	}
}

// pruneKeys drops the keys no head needs any more: only the current
// epoch's, and those that heads still known are wrapped under, stay.
func (st *state) pruneKeys() {
	if st.Roster == nil {
		return
	}
	need := map[uint64]bool{st.Roster.Epoch: true}
	for _, b := range st.Bundles {
		if b.Head != nil && !b.Head.Deleted {
			need[b.Head.Epoch] = true
		}
	}
	for _, p := range st.Published {
		if p.Head != nil {
			need[p.Head.Epoch] = true
		}
	}
	keep := st.Keys[:0]
	for _, k := range st.Keys {
		if need[k.Epoch] {
			keep = append(keep, k)
		}
	}
	st.Keys = keep
}

func (st *state) bundle(id string) *bundle {
	b := st.Bundles[id]
	if b == nil {
		b = &bundle{}
		st.Bundles[id] = b
	}
	return b
}

// clone copies st for a reader: the maps, slices and records are new.
// The heads, metadata and rosters they point to are shared, as the team's
// operations replace those and never change them.
func (st *state) clone() *state {
	c := *st
	c.Keys = append([]epochKey(nil), st.Keys...)
	c.Invites = append([]invite(nil), st.Invites...)
	c.Subs = append([]sub(nil), st.Subs...)
	c.Published = make(map[string]*published, len(st.Published))
	for k, v := range st.Published {
		p := *v
		c.Published[k] = &p
	}
	c.Bundles = make(map[string]*bundle, len(st.Bundles))
	for k, v := range st.Bundles {
		b := *v
		c.Bundles[k] = &b
	}
	return &c
}

func loadState(path string) (*state, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	st := &state{}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if st.V != 1 {
		return nil, fmt.Errorf("%s: state version %d not supported", path, st.V)
	}
	if st.Published == nil {
		st.Published = map[string]*published{}
	}
	if st.Bundles == nil {
		st.Bundles = map[string]*bundle{}
	}
	return st, nil
}

// load opens the team in dir.
func (g *Gateway) load(dir string) (*teamRT, error) {
	key, err := team.LoadMemberKey(filepath.Join(dir, keyFile))
	if err != nil {
		return nil, err
	}
	st, err := loadState(filepath.Join(dir, stateFile))
	if err != nil {
		return nil, err
	}
	if st.Team != key.Team || filepath.Base(dir) != key.Team {
		return nil, errors.New("member key and state are for different teams")
	}
	return g.newTeam(dir, key, st), nil
}

// save writes t's state and makes it what readers see. The caller holds
// t.op.
func (t *teamRT) save() error {
	t.st.pruneKeys()
	snap := t.st.clone()
	t.mu.Lock()
	t.snap = snap
	gone := t.gone
	t.mu.Unlock()
	if gone {
		return nil
	}
	b, err := json.MarshalIndent(t.st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(t.dir, stateFile), append(b, '\n'))
}

// pendingPath is where the held-back update of bundle id is kept.
func (t *teamRT) pendingPath(id string) string {
	return filepath.Join(t.dir, pendingDir, id+".tar.gz")
}

func (t *teamRT) dropPending(id string, b *bundle) {
	b.Pending = nil
	if err := os.Remove(t.pendingPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.log.Warn("team: removing a pending update failed", "team", t.id, "bundle", id, "err", err)
	}
}

// writeFileAtomic writes data to a 0600 temp file next to path, syncs it
// and renames it into place, creating the parent folder (0700) if needed.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
