package gateway

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"

	"fleet/internal/skills"
	"fleet/internal/team"
)

// errSkip marks a bundle that failed verification: logged and left out,
// not a reason to stop the sync.
var errSkip = errors.New("skipped")

// syncHeads takes the heads of bundles ids from the hub, or of every
// bundle for nil. The caller holds t.op.
func (t *teamRT) syncHeads(ctx context.Context, ids []string) error {
	// First what the user deleted, so it is not installed again.
	t.storeTombstones()
	var heads []*team.Head
	if ids == nil {
		var err error
		if heads, err = t.client.Heads(ctx); err != nil {
			return err
		}
	} else {
		for _, id := range ids {
			if !team.ValidID(id) {
				continue
			}
			h, err := t.client.Head(ctx, id)
			if team.IsStatus(err, http.StatusNotFound) {
				continue // its owner left meanwhile
			}
			if err != nil {
				return err
			}
			heads = append(heads, h)
		}
	}
	for _, h := range heads {
		if err := t.takeHead(ctx, h); err != nil {
			return err
		}
	}
	return nil
}

// takeHead takes one head from the hub: checked against the trusted
// roster and the bundle's highest version, then installed, updated, held
// back for a pin or removed. The caller holds t.op.
func (t *teamRT) takeHead(ctx context.Context, h *team.Head) error {
	st := t.st
	if err := team.VerifyHead(h, st.Roster); err != nil {
		t.log.Warn("team: a bundle head from the hub does not verify; skipped", "team", t.id, "bundle", h.ID, "err", err)
		return nil
	}
	if b := st.Bundles[h.ID]; b != nil {
		if b.Owner != "" && b.Owner != h.Owner {
			t.log.Warn("team: a bundle head names another owner than before; skipped", "team", t.id, "bundle", h.ID)
			return nil
		}
		if h.Version < b.High {
			t.log.Warn("team: the hub offered an older version of a bundle; skipped", "team", t.id, "bundle", h.ID, "version", h.Version, "seen", b.High)
			return nil
		}
	}
	b := st.bundle(h.ID)
	b.Owner, b.High, b.Head = h.Owner, h.Version, h
	if h.Owner == t.key.ID {
		// Its own: never installed a second time.
		if !h.Deleted {
			if m, err := team.OpenMeta(h, st.key(h.Epoch)); err == nil {
				b.Meta = &m
			}
		}
		return nil
	}
	if h.Deleted {
		b.Meta, b.Problem = nil, ""
		if b.Pending != nil {
			t.dropPending(h.ID, b)
		}
		if inst, _ := t.installed(h.ID); inst != nil {
			if err := t.removeInstalled(h.ID); err != nil {
				return err
			}
			t.log.Info("team: removed a skill its publisher withdrew", "team", t.id, "skill", inst.Name)
		}
		return nil
	}
	K := st.key(h.Epoch)
	if K == nil {
		b.Problem = "sealed under an older team key: waiting for its publisher to re-key it"
		return nil
	}
	m, err := team.OpenMeta(h, K)
	if err != nil {
		t.log.Warn("team: a bundle's metadata does not open; skipped", "team", t.id, "bundle", h.ID, "err", err)
		b.Problem = "its metadata does not open"
		return nil
	}
	b.Meta, b.Problem = &m, ""
	if m.Kind != "skill" {
		b.Problem = "a " + m.Kind + ", which this fleet cannot install"
		return nil
	}
	return t.update(ctx, h.ID, b)
}

// update installs or updates bundle id if it is wanted: subscribed to, or
// installed already, and not dropped. A pinned one's newer version is held
// back instead. The caller holds t.op.
func (t *teamRT) update(ctx context.Context, id string, b *bundle) error {
	h, m := b.Head, b.Meta
	if h == nil || h.Deleted || m == nil || m.Kind != "skill" {
		return nil
	}
	inst, fork := t.installed(id)
	if fork != nil {
		if b.Pending != nil {
			t.dropPending(id, b)
		}
		return nil
	}
	if b.Dropped || inst == nil && !t.subscribed(h.Owner, m.Name) {
		return nil
	}
	if inst != nil && inst.Source.BundleVersion >= h.Version {
		if b.Pending != nil && b.Pending.Version <= inst.Source.BundleVersion {
			t.dropPending(id, b)
		}
		return nil
	}
	if b.Pinned && inst != nil {
		if b.Pending != nil && b.Pending.Version >= h.Version {
			return nil
		}
		data, err := t.fetch(ctx, h, b)
		if errors.Is(err, errSkip) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := writeFileAtomic(t.pendingPath(id), data); err != nil {
			return err
		}
		b.Pending = &pending{Version: h.Version, Meta: *m}
		t.log.Info("team: an update waits behind a pin", "team", t.id, "skill", inst.Name, "version", h.Version)
		return nil
	}
	data, err := t.fetch(ctx, h, b)
	if errors.Is(err, errSkip) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := t.install(ctx, id, h.Version, m.Name, data); err != nil {
		// The store's trouble (a full disk, say), not the hub's: this
		// bundle waits for the next sync, the others go on.
		t.log.Warn("team: installing a skill failed", "team", t.id, "bundle", id, "err", err)
	}
	return nil
}

// install puts version of bundle id, data its files, into the store. A
// skill forked here is left alone.
func (t *teamRT) install(ctx context.Context, id string, version int64, name string, data []byte) error {
	b := t.st.bundle(id)
	owner, _ := t.st.Roster.Member(b.Owner)
	src := skills.Source{
		Team: t.id, TeamName: teamName(t.st), Bundle: id, BundleVersion: version,
		Owner: b.Owner, OwnerName: memberName(t.st, owner),
	}
	installed, err := t.g.opts.Skills.InstallTeam(ctx, src, name, data)
	if errors.Is(err, skills.ErrForked) {
		t.log.Info("team: a team skill was changed here, so it no longer updates", "team", t.id, "skill", installed)
		err = nil
	}
	if err != nil {
		return err
	}
	if b.Pending != nil && b.Pending.Version <= version {
		t.dropPending(id, b)
	}
	return nil
}

// fetch downloads, checks and opens a head's data: the skill's files. A
// bundle whose data does not verify is errSkip.
func (t *teamRT) fetch(ctx context.Context, h *team.Head, b *bundle) ([]byte, error) {
	data, err := t.client.Data(ctx, h.DataRef)
	if err != nil {
		return nil, err
	}
	_, files, err := team.OpenBundle(h, t.st.key(h.Epoch), data)
	if err != nil {
		t.log.Warn("team: a bundle's data does not verify; skipped", "team", t.id, "bundle", h.ID, "err", err)
		b.Problem = "its data does not match its head"
		return nil, errSkip
	}
	return files, nil
}

// installed finds the skill installed from bundle id, or the fork of it.
func (t *teamRT) installed(id string) (inst, fork *skills.Entry) {
	for _, e := range t.g.opts.Skills.TeamSkills(t.id) {
		switch {
		case e.Deleted:
		case e.Source != nil && e.Source.Bundle == id:
			inst = &e
		case e.ForkedFrom != nil && e.ForkedFrom.Bundle == id:
			fork = &e
		}
	}
	return inst, fork
}

// removeInstalled deletes the skill installed from bundle id, and takes
// note of the tombstone that leaves so it does not read as the user's.
func (t *teamRT) removeInstalled(id string) error {
	if err := t.g.opts.Skills.RemoveTeam(t.id, id); err != nil && !errors.Is(err, skills.ErrNotFound) {
		return err
	}
	b := t.st.bundle(id)
	for _, e := range t.g.opts.Skills.TeamSkills(t.id) {
		if e.Deleted && e.Source != nil && e.Source.Bundle == id {
			b.TombAck = max(b.TombAck, e.Version)
		}
	}
	return nil
}

// storeTombstones takes the team skills deleted from the store, here or on
// another machine of the fleet, as unsubscribed. The caller holds t.op.
func (t *teamRT) storeTombstones() {
	for _, e := range t.g.opts.Skills.TeamSkills(t.id) {
		if !e.Deleted || e.Source == nil {
			continue
		}
		b := t.st.bundle(e.Source.Bundle)
		if e.Version <= b.TombAck {
			continue
		}
		b.TombAck = e.Version
		if b.Owner == "" {
			b.Owner = e.Source.Owner
		}
		if !b.Dropped {
			b.Dropped = true
			t.log.Info("team: a team skill was deleted here, so it is unsubscribed", "team", t.id, "skill", e.Name)
		}
		if b.Pending != nil {
			t.dropPending(e.Source.Bundle, b)
		}
	}
}

// subscribed reports whether a subscription takes owner's skill name.
func (t *teamRT) subscribed(owner, name string) bool {
	return subscribedIn(t.st.Subs, owner, name)
}

// parseRef splits "<owner>/<name>" (name may be "*") and finds the owner.
// The caller holds t.op.
func (t *teamRT) parseRef(ref string) (owner, name string, err error) {
	o, n, ok := strings.Cut(strings.TrimSpace(ref), "/")
	if !ok || o == "" || n == "" {
		return "", "", errorf(Invalid, "say <member>/<skill> or <member>/*, not %q", ref)
	}
	if n != "*" && !skills.ValidName(n) {
		return "", "", errorf(Invalid, "%q is not a skill name", n)
	}
	if owner, err = t.member(o); err != nil {
		return "", "", err
	}
	return owner, n, nil
}

// Subscribe subscribes to "<owner>/<name>", or "<owner>/*" for all the
// owner publishes, in team ref, and installs what matches now. It also
// takes back the skills it matches that were unsubscribed or deleted.
func (g *Gateway) Subscribe(ctx context.Context, ref, skill string) error {
	return g.memberOp(ctx, ref, func(t *teamRT) error {
		owner, name, err := t.parseRef(skill)
		if err != nil {
			return err
		}
		if owner == t.key.ID {
			return errorf(Invalid, "those are this fleet's own skills")
		}
		s := sub{Owner: owner, Name: name}
		if !slices.Contains(t.st.Subs, s) {
			t.st.Subs = append(t.st.Subs, s)
		}
		// What was published since the last sync, too. The subscription
		// stands even if the hub cannot be reached: the loop installs.
		if err := t.syncHeads(ctx, nil); err != nil {
			return err
		}
		for _, id := range t.bundlesOf(owner, name) {
			b := t.st.Bundles[id]
			b.Dropped = false
			if err := t.update(ctx, id, b); err != nil {
				return err
			}
		}
		return nil
	})
}

// Unsubscribe ends a subscription ("<owner>/<name>" or "<owner>/*") in
// team ref and removes the skills it no longer takes. A skill a wildcard
// still matches is dropped by name.
func (g *Gateway) Unsubscribe(ctx context.Context, ref, skill string) error {
	return g.memberOp(ctx, ref, func(t *teamRT) error {
		owner, name, err := t.parseRef(skill)
		if err != nil {
			return err
		}
		n := len(t.st.Subs)
		t.st.Subs = slices.DeleteFunc(t.st.Subs, func(s sub) bool { return s.Owner == owner && s.Name == name })
		ids := t.bundlesOf(owner, name)
		if len(t.st.Subs) == n && len(ids) == 0 {
			return errorf(NotFound, "not subscribed to %s", skill)
		}
		for _, id := range ids {
			b := t.st.Bundles[id]
			if name == "*" && t.subscribed(owner, b.Meta.Name) {
				continue // still subscribed to by name
			}
			b.Dropped = true
			if b.Pending != nil {
				t.dropPending(id, b)
			}
			if inst, _ := t.installed(id); inst != nil {
				if err := t.removeInstalled(id); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// bundlesOf lists the live bundles of owner called name ("*": all). The
// caller holds t.op.
func (t *teamRT) bundlesOf(owner, name string) []string {
	var out []string
	for id, b := range t.st.Bundles {
		if b.Owner == owner && b.Head != nil && !b.Head.Deleted && b.Meta != nil && (name == "*" || b.Meta.Name == name) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// bundleRef finds a bundle by id, by "<owner>/<name>", or by the name of
// the skill it is installed as. The caller holds t.op.
func (t *teamRT) bundleRef(ref string) (string, *bundle, error) {
	ref = strings.TrimSpace(ref)
	if b, ok := t.st.Bundles[ref]; ok && team.ValidID(ref) {
		return ref, b, nil
	}
	if strings.Contains(ref, "/") {
		owner, name, err := t.parseRef(ref)
		if err != nil {
			return "", nil, err
		}
		if name != "*" {
			if ids := t.bundlesOf(owner, name); len(ids) == 1 {
				return ids[0], t.st.Bundles[ids[0]], nil
			}
		}
	} else {
		for _, e := range t.g.opts.Skills.TeamSkills(t.id) {
			if !e.Deleted && e.Source != nil && e.Name == ref {
				return e.Source.Bundle, t.st.bundle(e.Source.Bundle), nil
			}
		}
	}
	return "", nil, errorf(NotFound, "no team skill %q", ref)
}

// Pin keeps the installed version of a team skill (a bundle id,
// "<owner>/<name>" or its installed name) in team ref: updates wait until
// applied. Unpinning installs the latest version.
func (g *Gateway) Pin(ctx context.Context, ref, skill string, pin bool) error {
	return g.memberOp(ctx, ref, func(t *teamRT) error {
		id, b, err := t.bundleRef(skill)
		if err != nil {
			return err
		}
		if b.Owner == t.key.ID {
			return errorf(Invalid, "that is this fleet's own skill")
		}
		b.Pinned = pin
		if !pin {
			return t.update(ctx, id, b)
		}
		return nil
	})
}

// Apply installs the update a pin holds back for a team skill in team
// ref; it stays pinned.
func (g *Gateway) Apply(ctx context.Context, ref, skill string) error {
	return g.memberOp(ctx, ref, func(t *teamRT) error {
		id, b, err := t.bundleRef(skill)
		if err != nil {
			return err
		}
		if b.Pending == nil {
			return errorf(Conflict, "no update waits for %s", skill)
		}
		data, err := os.ReadFile(t.pendingPath(id))
		if err != nil {
			t.dropPending(id, b)
			return errorf(Conflict, "the pending update is gone; it comes again with the next sync")
		}
		p := b.Pending
		if err := t.install(ctx, id, p.Version, p.Meta.Name, data); err != nil {
			return errorf(Conflict, "installing the update failed: %v", err)
		}
		t.dropPending(id, b)
		return nil
	})
}

// teamName opens the team's name; its id if that fails.
func teamName(st *state) string {
	if st.Roster != nil {
		if K := st.current(); K != nil {
			if n, err := team.OpenTeamName(K, st.Team, st.Roster.TeamNameSealed); err == nil {
				return n
			}
		}
	}
	return st.Team[:8]
}
