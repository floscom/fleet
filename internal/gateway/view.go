package gateway

import (
	"cmp"
	"slices"
	"strings"

	"fleet/internal/skills"
)

// TeamView describes a team for the dashboard and the CLI. Times are Unix
// milliseconds.
type TeamView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Hub  string `json:"hub"`
	// Status is StatusOK, StatusWaiting, StatusOffline or StatusRemoved;
	// Error the last problem talking to the hub.
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	SyncedAtMs int64  `json:"syncedAtMs,omitempty"`
	Me         Member `json:"me"`
	// Admin is whether this fleet is an admin of the team.
	Admin   bool     `json:"admin"`
	Epoch   uint64   `json:"epoch"`
	Members []Member `json:"members"`
	// Invites are this fleet's open invites (admins).
	Invites []InviteView `json:"invites"`
	// Skills are the skills members published (this fleet's own too), and
	// installed ones whose publisher left.
	Skills []SkillView `json:"skills"`
	// Published are this fleet's own skills published to the team.
	Published []PublishedView `json:"published"`
	// Subscriptions are "<member>/<skill>" or "<member>/*".
	Subscriptions []string `json:"subscriptions"`
}

// Member is a member of a team.
type Member struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
	Me    bool   `json:"me,omitempty"`
}

// InviteView is an open invite; its id is the hub's (the hash of its
// credential), shortened.
type InviteView struct {
	ID        string `json:"id"`
	ExpiresMs int64  `json:"expiresMs"`
}

// SkillView is a skill published in the team.
type SkillView struct {
	Bundle string `json:"bundle"`
	// Ref is "<owner name>/<skill name>", what subscribe, pin, diff and
	// apply take.
	Ref         string `json:"ref"`
	Owner       string `json:"owner"`
	OwnerName   string `json:"ownerName"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Size        int64  `json:"size"`
	Files       int    `json:"files"`
	// ChangedMs is when its publisher last changed it (its version).
	ChangedMs int64 `json:"changedMs"`
	// Mine is set for this fleet's own skills.
	Mine bool `json:"mine,omitempty"`
	// Subscribed is whether a subscription takes it; Installed the name it
	// is installed under here, InstalledMs the version installed.
	Subscribed  bool   `json:"subscribed"`
	Installed   string `json:"installed,omitempty"`
	InstalledMs int64  `json:"installedMs,omitempty"`
	Pinned      bool   `json:"pinned,omitempty"`
	// Pending is the newer version a pin holds back.
	Pending *PendingView `json:"pending,omitempty"`
	// Forked is the name of the user's own skill that was this one until
	// it was changed here: it no longer updates.
	Forked string `json:"forked,omitempty"`
	// Dropped is set for one unsubscribed or deleted here: it is not
	// installed until subscribed to again.
	Dropped bool `json:"dropped,omitempty"`
	// PublisherLeft is set for an installed skill whose publisher is no
	// longer in the team: it stays, and no longer updates.
	PublisherLeft bool `json:"publisherLeft,omitempty"`
	// Problem says why it cannot be installed.
	Problem string `json:"problem,omitempty"`
}

// PendingView is an update held back by a pin.
type PendingView struct {
	ChangedMs   int64  `json:"changedMs"`
	Size        int64  `json:"size"`
	Description string `json:"description,omitempty"`
}

// PublishedView is an own skill published to the team.
type PublishedView struct {
	Skill  string `json:"skill"`
	Bundle string `json:"bundle"`
	// ChangedMs is the version on the hub; 0 before the first upload.
	ChangedMs int64 `json:"changedMs,omitempty"`
	// Status is "published", "changing" (sent once it holds still),
	// "pending" (not uploaded yet) or "error".
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// name is the team's name, as last saved.
func (t *teamRT) name() string {
	t.mu.Lock()
	st := t.snap
	t.mu.Unlock()
	return teamName(st)
}

// view describes t as last saved.
func (t *teamRT) view() TeamView {
	t.mu.Lock()
	st := t.snap
	v := TeamView{ID: t.id, Hub: st.Hub, Status: t.status, Error: t.lastErr, SyncedAtMs: t.syncedAt.UnixMilli()}
	if t.syncedAt.IsZero() {
		v.SyncedAtMs = 0
	}
	t.mu.Unlock()
	v.Name = teamName(st)
	v.Me = Member{ID: t.key.ID, Name: st.Me, Me: true}
	v.Members, v.Invites, v.Skills, v.Published, v.Subscriptions = []Member{}, []InviteView{}, []SkillView{}, []PublishedView{}, []string{}
	r := st.Roster
	if r == nil {
		return v
	}
	v.Epoch = r.Epoch
	v.Admin = r.IsAdmin(t.key.ID)
	names := map[string]string{}
	for _, m := range r.Members {
		mv := Member{ID: m.ID, Name: memberName(st, m), Admin: m.Admin, Me: m.ID == t.key.ID}
		names[m.ID] = mv.Name
		if mv.Me {
			v.Me = mv
		}
		v.Members = append(v.Members, mv)
	}
	slices.SortFunc(v.Members, func(a, b Member) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.ID, b.ID))
	})
	for _, in := range st.Invites {
		v.Invites = append(v.Invites, InviteView{ID: in.ID[:12], ExpiresMs: in.Expires.UnixMilli()})
	}
	for _, s := range st.Subs {
		v.Subscriptions = append(v.Subscriptions, cmp.Or(names[s.Owner], s.Owner[:8])+"/"+s.Name)
	}
	slices.Sort(v.Subscriptions)
	for name, p := range st.Published {
		pv := PublishedView{Skill: name, Bundle: p.Bundle, Error: p.Error}
		switch {
		case p.Error != "":
			pv.Status = "error"
		case p.waiting:
			pv.Status = "changing"
		case p.Head == nil:
			pv.Status = "pending"
		default:
			pv.Status = "published"
		}
		if p.Head != nil {
			pv.ChangedMs = p.Head.Version
		}
		v.Published = append(v.Published, pv)
	}
	slices.SortFunc(v.Published, func(a, b PublishedView) int { return strings.Compare(a.Skill, b.Skill) })

	inst := map[string]skills.Entry{}
	forks := map[string]skills.Entry{}
	for _, e := range t.g.opts.Skills.TeamSkills(t.id) {
		switch {
		case e.Deleted:
		case e.Source != nil:
			inst[e.Source.Bundle] = e
		case e.ForkedFrom != nil:
			forks[e.ForkedFrom.Bundle] = e
		}
	}
	for id, b := range st.Bundles {
		e, installed := inst[id]
		_, member := r.Member(b.Owner)
		up := b.Head != nil && !b.Head.Deleted && member
		live := up && b.Meta != nil
		// Shown: what is published, what waits to be opened, and what is
		// installed from a publisher who left.
		if !live && !(up && b.Problem != "") && !(installed && !member) {
			continue
		}
		sv := SkillView{
			Bundle: id, Owner: b.Owner, OwnerName: cmp.Or(names[b.Owner], b.Owner[:min(8, len(b.Owner))]),
			Mine: b.Owner == t.key.ID, Pinned: b.Pinned, Dropped: b.Dropped, Problem: b.Problem,
			PublisherLeft: !member,
		}
		if live {
			m := b.Meta
			sv.Name, sv.Description, sv.Size, sv.Files, sv.ChangedMs = m.Name, m.Description, m.Size, len(m.Files), b.Head.Version
		} else if installed {
			sv.Name, sv.Size, sv.ChangedMs = e.Source.Name, e.Size, e.Source.BundleVersion
			if !member {
				sv.OwnerName = cmp.Or(e.Source.OwnerName, sv.OwnerName)
			}
		}
		if sv.Name == "" {
			sv.Name = "(sealed)"
		}
		sv.Ref = sv.OwnerName + "/" + sv.Name
		if installed {
			sv.Installed, sv.InstalledMs = e.Name, e.Source.BundleVersion
		}
		if f, ok := forks[id]; ok {
			sv.Forked = f.Name
		}
		if b.Pending != nil {
			sv.Pending = &PendingView{ChangedMs: b.Pending.Version, Size: b.Pending.Meta.Size, Description: b.Pending.Meta.Description}
		}
		sv.Subscribed = !sv.Mine && !b.Dropped && (installed || subscribedIn(st.Subs, b.Owner, sv.Name))
		v.Skills = append(v.Skills, sv)
	}
	// Installed skills of members who left that no bundle record knows
	// (the state was lost, say).
	for id, e := range inst {
		if _, ok := st.Bundles[id]; ok {
			continue
		}
		_, member := r.Member(e.Source.Owner)
		v.Skills = append(v.Skills, SkillView{
			Bundle: id, Ref: e.Source.Ref(), Owner: e.Source.Owner, OwnerName: cmp.Or(e.Source.OwnerName, e.Source.Owner),
			Name: e.Source.Name, Size: e.Size, ChangedMs: e.Source.BundleVersion, Subscribed: true,
			Installed: e.Name, InstalledMs: e.Source.BundleVersion, PublisherLeft: !member,
		})
	}
	slices.SortFunc(v.Skills, func(a, b SkillView) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Ref), strings.ToLower(b.Ref)), strings.Compare(a.Bundle, b.Bundle))
	})
	return v
}

func subscribedIn(subs []sub, owner, name string) bool {
	return slices.ContainsFunc(subs, func(s sub) bool { return s.Owner == owner && (s.Name == "*" || s.Name == name) })
}
