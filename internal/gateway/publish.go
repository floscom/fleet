package gateway

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"fleet/internal/skills"
	"fleet/internal/team"
)

// ownerDuties brings what this member publishes up to date with the skill
// store: a new version of a skill that changed, a re-wrap of one under an
// old epoch's key, a tombstone for one deleted. A skill still changing is
// left for the next round (published.waiting). The caller holds t.op.
func (t *teamRT) ownerDuties(ctx context.Context) error {
	st := t.st
	if st.Roster == nil || st.Removed || st.current() == nil {
		return nil
	}
	names := make([]string, 0, len(st.Published))
	for n := range st.Published {
		names = append(names, n)
	}
	slices.Sort(names)
	var first error
	for _, name := range names {
		p := st.Published[name]
		err := t.syncPublished(ctx, name, p)
		if err != nil {
			p.Error = err.Error()
			t.log.Warn("team: publishing a skill failed", "team", t.id, "skill", name, "err", err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// syncPublished publishes skill name as the store has it now, if that is
// not what the team has. The caller holds t.op.
func (t *teamRT) syncPublished(ctx context.Context, name string, p *published) error {
	st := t.st
	st.Published[name] = p
	p.waiting = false
	e, ok := t.g.opts.Skills.Entry(name)
	if !ok || e.Deleted || e.Source != nil {
		// Deleted, or a team skill of that name took its place.
		if err := t.withdraw(ctx, p); err != nil {
			return err
		}
		delete(st.Published, name)
		t.log.Info("team: withdrew a deleted skill", "team", t.id, "skill", name)
		return nil
	}
	K := st.current()
	if p.Head != nil && p.Hash == e.Hash {
		if p.Head.Epoch == st.Roster.Epoch {
			p.Error = ""
			return nil
		}
		if oldK := st.key(p.Head.Epoch); oldK != nil {
			return t.putHead(ctx, p, func(version int64) (*team.Head, []byte, error) {
				h, err := team.Rewrap(p.Head, t.key, oldK, version, st.Roster.Epoch, K)
				return h, nil, err
			})
		}
		// The old key is gone: seal it afresh.
	}
	e, data, err := t.g.opts.Skills.Archive(name)
	if errors.Is(err, skills.ErrChanging) {
		p.waiting = true
		return nil
	}
	if err != nil {
		return err
	}
	files, err := readArchive(data)
	if err != nil {
		return err
	}
	meta := team.Meta{Kind: "skill", Name: name, Description: describe(files[skillFile]), Hash: e.Hash, Size: e.Size}
	for f := range files {
		meta.Files = append(meta.Files, f)
	}
	slices.Sort(meta.Files)
	err = t.putHead(ctx, p, func(version int64) (*team.Head, []byte, error) {
		return team.SealBundle(t.key, p.Bundle, version, st.Roster.Epoch, K, meta, data)
	})
	if err == nil {
		p.Hash = e.Hash
		t.log.Info("team: published a skill", "team", t.id, "skill", name, "version", p.Head.Version)
	}
	return err
}

// putHead uploads the head (and data) that build makes for the next
// version of p's bundle. When the hub has a higher version than this
// member knew of (its state was restored, say), it learns that one and
// tries once more above it. The caller holds t.op.
func (t *teamRT) putHead(ctx context.Context, p *published, build func(version int64) (*team.Head, []byte, error)) error {
	b := t.st.bundle(p.Bundle)
	b.Owner = t.key.ID
	for attempt := 0; ; attempt++ {
		prev := b.High
		if p.Head != nil {
			prev = max(prev, p.Head.Version)
		}
		h, data, err := build(team.NextVersion(prev))
		if err != nil {
			return err
		}
		if data != nil {
			if err := t.client.PutData(ctx, h.DataRef, data); err != nil {
				return err
			}
		}
		err = t.client.PutHead(ctx, h)
		if err == nil {
			p.Head, p.Error = h, ""
			b.High, b.Head = h.Version, h
			if !h.Deleted {
				if m, err := team.OpenMeta(h, t.st.key(h.Epoch)); err == nil {
					b.Meta = &m
				}
			}
			return nil
		}
		if !team.IsStatus(err, http.StatusConflict) || attempt > 0 {
			return err
		}
		cur, herr := t.client.Head(ctx, p.Bundle)
		if herr != nil {
			return err
		}
		if team.VerifyHead(cur, t.st.Roster) != nil || cur.Owner != t.key.ID {
			return err
		}
		b.High = max(b.High, cur.Version)
	}
}

// withdraw puts a tombstone for p's bundle, if it was ever uploaded.
func (t *teamRT) withdraw(ctx context.Context, p *published) error {
	if p.Head == nil || p.Head.Deleted {
		return nil
	}
	return t.putHead(ctx, p, func(version int64) (*team.Head, []byte, error) {
		h, err := team.Tombstone(p.Head, t.key, version)
		return h, nil, err
	})
}

// Publish publishes skill, one of the user's own, to team ref. From then
// on every change to it goes to the team, until it is unpublished or
// deleted. A skill still changing is published once it holds still.
func (g *Gateway) Publish(ctx context.Context, ref, skill string) error {
	return g.memberOp(ctx, ref, func(t *teamRT) error {
		e, ok := g.opts.Skills.Entry(skill)
		if !ok || e.Deleted {
			return errorf(NotFound, "no skill %q in this fleet's skills", skill)
		}
		if e.Source != nil {
			return errorf(Forbidden, "%s is %s's, from team %s: only they publish it (change it to make it your own)", skill, e.Source.Ref(), orID(e.Source.TeamName, e.Source.Team))
		}
		if _, ok := t.st.Published[skill]; ok {
			return nil
		}
		p := &published{Bundle: team.NewID()}
		err := t.syncPublished(ctx, skill, p)
		if err == nil {
			return nil
		}
		var he *team.HubError
		if !errors.As(err, &he) && !isOffline(err) {
			// Nothing the hub could fix (too large, say): not published.
			delete(t.st.Published, skill)
			return errorf(Invalid, "%s: %s", skill, strings.TrimPrefix(err.Error(), "team: "))
		}
		// Kept: the loop tries again.
		p.Error = err.Error()
		return err
	})
}

// Unpublish withdraws skill from team ref; members who installed it lose
// it.
func (g *Gateway) Unpublish(ctx context.Context, ref, skill string) error {
	return g.memberOp(ctx, ref, func(t *teamRT) error {
		p, ok := t.st.Published[skill]
		if !ok {
			return errorf(NotFound, "%s is not published to the team", skill)
		}
		if err := t.withdraw(ctx, p); err != nil {
			return err
		}
		delete(t.st.Published, skill)
		return nil
	})
}

func orID(name, id string) string {
	if name != "" {
		return name
	}
	return id
}
