package gateway

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"fleet/internal/team"
)

// maxRetries bounds the attempts at a roster post that keeps losing races
// with other admins.
const maxRetries = 3

// MaxInviteTTL is the longest an invite may be good for (the hub's limit).
const MaxInviteTTL = 30 * 24 * time.Hour

// adminDuties adds whoever joined with one of this admin's invites and
// drops the members who left. Nothing for a member who is no admin. The
// caller holds t.op.
func (t *teamRT) adminDuties(ctx context.Context) error {
	st := t.st
	if st.Roster == nil || !st.Roster.IsAdmin(t.key.ID) {
		return nil
	}
	jl, err := t.client.Joins(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	asked := map[string]bool{}
	for _, j := range jl.Joins {
		asked[j.Invite] = true
		i := slices.IndexFunc(st.Invites, func(in invite) bool { return in.ID == j.Invite })
		if i < 0 {
			continue // another admin's invite
		}
		in := st.Invites[i]
		inv := &team.Invite{Hub: st.Hub, Team: st.Team, Secret: in.Secret, Admin: t.key.ID}
		m, name, err := inv.VerifyJoin(j)
		if err == nil && m.ID != j.ID {
			err = errors.New("the hub's id for the joiner does not match its keys")
		}
		// The invite is spent either way: the hub marked it used.
		st.Invites = slices.Delete(st.Invites, i, i+1)
		if err != nil {
			t.log.Warn("team: refused a join request that does not match its invite", "team", t.id, "member", j.ID, "err", err)
			if derr := t.client.DeleteJoin(ctx, j.ID); derr != nil && !team.IsStatus(derr, http.StatusNotFound) {
				return derr
			}
			continue
		}
		if err := t.addMember(ctx, m, cleanName(name, m.ID)); err != nil {
			// Try again with the next sync: keep the secret.
			st.Invites = append(st.Invites, in)
			return err
		}
	}
	// A secret is kept until its invite expired and no join waits on it.
	st.Invites = slices.DeleteFunc(st.Invites, func(in invite) bool { return !now.Before(in.Expires) && !asked[in.ID] })
	for _, id := range jl.Left {
		if _, ok := st.Roster.Member(id); !ok {
			continue
		}
		if err := t.removeMember(ctx, id); err != nil {
			return err
		}
		t.log.Info("team: removed a member who left", "team", t.id, "member", id)
	}
	return nil
}

// changeRoster posts the roster that build makes on the trusted head, with
// the keys it needs, and makes it the head. A post that loses a race with
// another admin fetches the chain and tries again; build may then report
// that nothing is left to do (nil roster). The caller holds t.op.
func (t *teamRT) changeRoster(ctx context.Context, build func(prev *team.Roster) (*team.Roster, []team.SealedKey, []byte, error)) error {
	for attempt := 0; ; attempt++ {
		prev := t.st.Roster
		next, keys, newK, err := build(prev)
		if err != nil || next == nil {
			return err
		}
		if err := team.VerifyNext(prev, next); err != nil {
			return err // a bug here, not the hub's doing
		}
		err = t.client.PostRoster(ctx, next, keys)
		if err == nil {
			t.st.Roster = next
			if newK != nil {
				t.st.addKey(next.Epoch, newK)
			}
			return nil
		}
		if !team.IsStatus(err, http.StatusConflict) || attempt >= maxRetries {
			return err
		}
		if _, err := t.syncRoster(ctx); err != nil {
			return err
		}
		if t.st.Removed || !t.st.Roster.IsAdmin(t.key.ID) {
			return errorf(Forbidden, "this member is no longer an admin")
		}
	}
}

// addMember adds a verified joiner, with its sealed key.
func (t *teamRT) addMember(ctx context.Context, m team.Member, name string) error {
	err := t.changeRoster(ctx, func(prev *team.Roster) (*team.Roster, []team.SealedKey, []byte, error) {
		if _, ok := prev.Member(m.ID); ok {
			return nil, nil, nil, nil // another admin was quicker
		}
		K := t.st.key(prev.Epoch)
		if K == nil {
			return nil, nil, nil, errorf(Conflict, "no team key for epoch %d", prev.Epoch)
		}
		next, err := team.AddMember(prev, t.key, K, m, name)
		if err != nil {
			return nil, nil, nil, err
		}
		sk, err := team.SealTeamKey(next.Team, next.Epoch, K, m)
		if err != nil {
			return nil, nil, nil, err
		}
		return next, []team.SealedKey{sk}, nil, nil
	})
	if err == nil {
		t.log.Info("team: member added", "team", t.id, "member", m.ID)
	}
	return err
}

// removeMember drops id from the roster in a new epoch, with the new key
// sealed for everyone left, and re-wraps this member's own heads.
func (t *teamRT) removeMember(ctx context.Context, id string) error {
	err := t.changeRoster(ctx, func(prev *team.Roster) (*team.Roster, []team.SealedKey, []byte, error) {
		if _, ok := prev.Member(id); !ok {
			return nil, nil, nil, nil
		}
		K := t.st.key(prev.Epoch)
		if K == nil {
			return nil, nil, nil, errorf(Conflict, "no team key for epoch %d", prev.Epoch)
		}
		next, newK, err := team.RemoveMember(prev, t.key, K, id)
		if err != nil {
			return nil, nil, nil, errorf(Conflict, "%s", strings.TrimPrefix(err.Error(), "team: "))
		}
		keys, err := team.SealKeys(next, newK)
		if err != nil {
			return nil, nil, nil, err
		}
		return next, keys, newK, nil
	})
	if err != nil {
		return err
	}
	t.membersLeft()
	return t.ownerDuties(ctx)
}

// adminOp runs fn for an admin of team ref, then saves.
func (g *Gateway) adminOp(ctx context.Context, ref string, fn func(t *teamRT) error) error {
	return g.memberOp(ctx, ref, func(t *teamRT) error {
		if !t.st.Roster.IsAdmin(t.key.ID) {
			return errorf(Forbidden, "only an admin of the team can do that")
		}
		return fn(t)
	})
}

// memberOp runs fn for team ref, which this fleet must be a member of
// now, then saves and notes the outcome.
func (g *Gateway) memberOp(ctx context.Context, ref string, fn func(t *teamRT) error) error {
	t, err := g.find(ref)
	if err != nil {
		return err
	}
	t.op.Lock()
	defer t.op.Unlock()
	switch {
	case t.st.Removed:
		return errorf(Conflict, "the hub no longer takes this fleet's requests for the team: it was removed")
	case t.st.Roster == nil:
		return errorf(Conflict, "not a member yet: waiting for an admin's fleet to add this one")
	}
	err = fn(t)
	if serr := t.save(); serr != nil && err == nil {
		err = serr
	}
	var ge *Error
	if err != nil && !errors.As(err, &ge) {
		t.noteErr(err)
	}
	return hubErr(err)
}

// Invite makes an invite to team ref good for ttl (0: 7 days), and
// returns it. It is the approval too: whoever joins with it is added by
// this fleet's gateway.
func (g *Gateway) Invite(ctx context.Context, ref string, ttl time.Duration) (inv string, expires time.Time, err error) {
	if ttl == 0 {
		ttl = team.DefaultInviteTTL
	}
	if ttl < time.Minute || ttl > MaxInviteTTL {
		return "", time.Time{}, errorf(Invalid, "an invite is good for a minute to 30 days")
	}
	err = g.adminOp(ctx, ref, func(t *teamRT) error {
		in, err := team.NewInvite(t.st.Hub, t.id, t.key.ID)
		if err != nil {
			return err
		}
		expires = time.Now().Add(ttl).Truncate(time.Second)
		if err := t.client.RegisterInvite(ctx, in, expires); err != nil {
			return err
		}
		t.st.Invites = append(t.st.Invites, invite{ID: in.ID(), Secret: in.Secret, Expires: expires})
		inv = in.String()
		return nil
	})
	return inv, expires, err
}

// SetAdmin makes member (a name or id prefix) of team ref an admin or
// not.
func (g *Gateway) SetAdmin(ctx context.Context, ref, member string, admin bool) error {
	return g.adminOp(ctx, ref, func(t *teamRT) error {
		id, err := t.member(member)
		if err != nil {
			return err
		}
		return t.changeRoster(ctx, func(prev *team.Roster) (*team.Roster, []team.SealedKey, []byte, error) {
			m, ok := prev.Member(id)
			if !ok {
				return nil, nil, nil, errorf(NotFound, "%s is no longer a member", member)
			}
			if m.Admin == admin {
				return nil, nil, nil, nil
			}
			next, err := team.SetAdmin(prev, t.key, id, admin)
			if err != nil {
				return nil, nil, nil, errorf(Conflict, "%s", strings.TrimPrefix(err.Error(), "team: "))
			}
			return next, nil, nil, nil
		})
	})
}

// Remove removes member (a name or id prefix) from team ref: a new epoch
// whose key it never gets.
func (g *Gateway) Remove(ctx context.Context, ref, member string) error {
	return g.adminOp(ctx, ref, func(t *teamRT) error {
		id, err := t.member(member)
		if err != nil {
			return err
		}
		if id == t.key.ID {
			return errorf(Invalid, "to remove this fleet from the team, leave it")
		}
		return t.removeMember(ctx, id)
	})
}

// Leave leaves team ref and forgets it. The skills installed from it stay
// as they are and no longer update.
func (g *Gateway) Leave(ctx context.Context, ref string) error {
	t, err := g.find(ref)
	if err != nil {
		return err
	}
	t.op.Lock()
	defer t.op.Unlock()
	if t.st.Roster != nil && !t.st.Removed {
		if err := t.client.Leave(ctx); err != nil {
			if team.IsStatus(err, http.StatusConflict) {
				return errorf(Conflict, "the last admin cannot leave: make someone else an admin first")
			}
			// A member the hub refuses is out already.
			if !team.IsStatus(err, http.StatusForbidden) {
				return hubErr(err)
			}
		}
	}
	// A joiner still waiting just forgets: its join request expires with
	// the invite, and nobody gets a key for it.
	t.mu.Lock()
	t.gone = true
	t.mu.Unlock()
	g.forget(t)
	g.log.Info("team: left", "team", t.id)
	return nil
}

// member resolves a member by name (any case), id or id prefix. The
// caller holds t.op.
func (t *teamRT) member(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errorf(Invalid, "which member?")
	}
	r := t.st.Roster
	var byName, byPrefix []string
	for _, m := range r.Members {
		if m.ID == ref {
			return m.ID, nil
		}
		if strings.EqualFold(t.memberName(m), ref) {
			byName = append(byName, m.ID)
		}
		if strings.HasPrefix(m.ID, strings.ToLower(ref)) {
			byPrefix = append(byPrefix, m.ID)
		}
	}
	for _, c := range [][]string{byName, byPrefix} {
		switch len(c) {
		case 0:
			continue
		case 1:
			return c[0], nil
		}
		return "", errorf(Invalid, "%q names %d members: use the member id", ref, len(c))
	}
	return "", errorf(NotFound, "no member %q in the team", ref)
}

// memberName opens a member's display name; its id if that fails.
func (t *teamRT) memberName(m team.Member) string {
	return memberName(t.st, m)
}

func memberName(st *state, m team.Member) string {
	if st.Roster != nil {
		if K := st.current(); K != nil {
			if n, err := team.OpenName(K, st.Team, m.ID, m.NameSealed); err == nil {
				return n
			}
		}
	}
	return m.ID[:8]
}
