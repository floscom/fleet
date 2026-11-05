package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"fleet/internal/team"
)

// Variables so tests can shorten them.
var (
	// fullSyncEvery is how often a team is synced in full even when the
	// events said nothing: a missed event costs no more than this.
	fullSyncEvery = 10 * time.Minute
	// waitPoll is how often a joiner asks whether an admin added it yet.
	waitPoll = 15 * time.Second
	// removedPoll is how often a member the hub refuses tries again.
	removedPoll = 10 * time.Minute
	// backoffMin and backoffMax bound the wait after a failed hub call.
	backoffMin = 2 * time.Second
	backoffMax = 5 * time.Minute
	// storePoll is how often the skill store is looked at for changes.
	storePoll = 2 * time.Second
	// retryPublish is how soon a skill that was changing is tried again.
	retryPublish = 3 * time.Second
)

// Statuses of a team.
const (
	StatusOK      = "ok"      // a member, the hub reachable
	StatusWaiting = "waiting" // joined, waiting for an admin's fleet to add this one
	StatusOffline = "offline" // the hub cannot be reached; the last state stands
	StatusRemoved = "removed" // the hub refuses this member: removed from the team
)

// teamRT is one team at run time.
type teamRT struct {
	g      *Gateway
	log    *slog.Logger
	id     string
	dir    string
	key    *team.MemberKey
	client *team.Client

	// op serializes everything that changes st or talks to the hub on its
	// behalf; st is only touched under it.
	op sync.Mutex
	st *state

	// mu guards what readers see: snap (st as last saved) and the status.
	mu       sync.Mutex
	snap     *state
	status   string
	lastErr  string
	syncedAt time.Time
	gone     bool

	// kick asks the loop for a full sync, storeKick to look at the skill
	// store.
	kick      chan struct{}
	storeKick chan struct{}
	cancel    context.CancelFunc
}

func (g *Gateway) newTeam(dir string, key *team.MemberKey, st *state) *teamRT {
	c := team.NewClient(key)
	if g.opts.HTTP != nil {
		c.HTTP = g.opts.HTTP
	}
	t := &teamRT{
		g: g, log: g.log, id: key.Team, dir: dir, key: key, client: c, st: st, snap: st.clone(),
		kick: make(chan struct{}, 1), storeKick: make(chan struct{}, 1),
	}
	switch {
	case st.Removed:
		t.status = StatusRemoved
	case st.Roster == nil:
		t.status = StatusWaiting
	default:
		t.status = StatusOK
	}
	return t
}

// poke signals ch without waiting.
func (t *teamRT) poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (t *teamRT) setStatus(status string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if status != "" {
		t.status = status
	}
	t.lastErr = ""
	if err != nil {
		t.lastErr = err.Error()
	}
	if err == nil && status == StatusOK {
		t.syncedAt = time.Now()
	}
}

// events is one long poll's outcome.
type events struct {
	list team.EventList
	err  error
}

// run is the team's loop: sync, then follow the events, until ctx ends.
func (t *teamRT) run(ctx context.Context) {
	evc := make(chan events, 1)
	polling := false
	full := true
	var lastFull time.Time
	backoff := time.Duration(0)
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	for ctx.Err() == nil {
		if full || time.Since(lastFull) >= fullSyncEvery {
			err := t.fullSync(ctx)
			if ctx.Err() != nil {
				return
			}
			wait := time.Duration(0)
			switch t.view().Status {
			case StatusWaiting:
				wait = waitPoll
			case StatusRemoved:
				wait = removedPoll
			}
			if err != nil && wait == 0 {
				backoff = min(max(2*backoff, backoffMin), backoffMax)
				wait = backoff
			} else if err == nil {
				backoff = 0
			}
			if wait > 0 {
				if !sleep(ctx, wait, t.kick) {
					return
				}
				continue
			}
			full, lastFull = false, time.Now()
		}
		if !polling {
			polling = true
			cursor := t.cursor()
			go func() {
				l, err := t.client.Events(ctx, cursor)
				evc <- events{l, err}
			}()
		}
		select {
		case <-ctx.Done():
			return
		case ev := <-evc:
			polling = false
			if ev.err != nil {
				if ctx.Err() != nil {
					return
				}
				t.log.Debug("team: events failed", "team", t.id, "err", ev.err)
				// Whatever happened meanwhile, a full sync after the pause
				// catches up on it.
				full = true
				backoff = min(max(2*backoff, backoffMin), backoffMax)
				if !sleep(ctx, backoff, t.kick) {
					return
				}
				continue
			}
			backoff = 0
			if err := t.handleEvents(ctx, ev.list); err != nil {
				full = true
			}
		case <-t.kick:
			full = true
		case <-t.storeKick:
			if again := t.storeChanged(ctx); again {
				retry.Reset(retryPublish)
			}
		case <-retry.C:
			if again := t.storeChanged(ctx); again {
				retry.Reset(retryPublish)
			}
		case <-time.After(time.Until(lastFull.Add(fullSyncEvery))):
			full = true
		}
	}
}

// sleep waits d, or less if kick is signalled; false if ctx ended.
func sleep(ctx context.Context, d time.Duration, kick <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
	case <-kick:
	}
	return true
}

func (t *teamRT) cursor() uint64 {
	t.op.Lock()
	defer t.op.Unlock()
	return t.st.Cursor
}

// handleEvents acts on one batch of events.
func (t *teamRT) handleEvents(ctx context.Context, l team.EventList) error {
	t.op.Lock()
	defer t.op.Unlock()
	if l.Reset {
		t.st.Cursor = l.Next
		return errors.New("event log reset")
	}
	var roster, admin bool
	bundles := map[string]bool{}
	for _, e := range l.Events {
		switch e.Kind {
		case team.EventRoster, team.EventKeys:
			roster = true
		case team.EventJoin, team.EventLeft:
			admin = true
		case team.EventBundle:
			bundles[e.Ref] = true
		}
	}
	var err error
	if roster {
		var epoch bool
		if epoch, err = t.syncRoster(ctx); err == nil && epoch {
			// Heads others re-wrap come as events of their own; the full
			// list catches heads that could not be opened before.
			bundles = nil
			err = t.syncHeads(ctx, nil)
		}
		if err == nil {
			err = t.ownerDuties(ctx)
		}
	}
	if err == nil && (admin || roster) {
		err = t.adminDuties(ctx)
	}
	if err == nil && len(bundles) > 0 {
		ids := make([]string, 0, len(bundles))
		for id := range bundles {
			ids = append(ids, id)
		}
		if len(ids) > maxHeadFetches {
			ids = nil // cheaper to take the whole list
		}
		err = t.syncHeads(ctx, ids)
	}
	if err == nil {
		t.st.Cursor = l.Next
	}
	if serr := t.save(); serr != nil && err == nil {
		err = serr
	}
	t.noteErr(err)
	return err
}

// maxHeadFetches is how many bundle events are fetched head by head; more
// fetch the list.
const maxHeadFetches = 4

// noteErr records the outcome of hub work in the status.
func (t *teamRT) noteErr(err error) {
	switch {
	case err == nil:
		t.setStatus(t.memberStatus(), nil)
	case isOffline(err):
		t.setStatus(StatusOffline, err)
	default:
		t.setStatus("", err)
	}
}

// memberStatus is the status the state gives. The caller holds t.op.
func (t *teamRT) memberStatus() string {
	switch {
	case t.st.Removed:
		return StatusRemoved
	case t.st.Roster == nil:
		return StatusWaiting
	}
	return StatusOK
}

// isOffline reports whether err means the hub could not be reached or is
// failing, rather than refusing something.
func isOffline(err error) bool {
	var he *team.HubError
	if errors.As(err, &he) {
		return he.Status >= 500 || he.Status == http.StatusTooManyRequests
	}
	var ge *Error
	return !errors.As(err, &ge) && !errors.Is(err, team.ErrVerify)
}

// fullSync syncs everything: the roster chain and key, an admin's joins
// and leavers, every head, and what this member publishes.
func (t *teamRT) fullSync(ctx context.Context) error {
	t.op.Lock()
	defer t.op.Unlock()
	err := t.fullSyncLocked(ctx)
	if serr := t.save(); serr != nil && err == nil {
		err = serr
	}
	t.noteErr(err)
	return err
}

func (t *teamRT) fullSyncLocked(ctx context.Context) error {
	if _, err := t.syncRoster(ctx); err != nil {
		return err
	}
	if t.st.Roster == nil || t.st.Removed {
		return nil
	}
	if err := t.adminDuties(ctx); err != nil {
		return err
	}
	if err := t.syncHeads(ctx, nil); err != nil {
		return err
	}
	return t.ownerDuties(ctx)
}

// storeChanged takes what changed in the skill store: tombstones of team
// skills, and own published skills to send again. again reports work left
// for later (a skill still changing).
func (t *teamRT) storeChanged(ctx context.Context) (again bool) {
	t.op.Lock()
	defer t.op.Unlock()
	if t.st.Roster == nil || t.st.Removed {
		return false
	}
	t.storeTombstones()
	err := t.ownerDuties(ctx)
	if serr := t.save(); serr != nil && err == nil {
		err = serr
	}
	if err != nil {
		t.log.Warn("team: publishing failed", "team", t.id, "err", err)
		return true
	}
	for _, p := range t.st.Published {
		if p.waiting {
			return true
		}
	}
	return false
}

// syncRoster advances the trusted roster as far as the hub's chain
// verifies, and fetches the team key of a new epoch. epoch reports that
// the epoch changed. The caller holds t.op.
func (t *teamRT) syncRoster(ctx context.Context) (epoch bool, err error) {
	st := t.st
	var since uint64
	if st.Roster != nil {
		since = st.Roster.Seq
	}
	rosters, err := t.client.Rosters(ctx, since)
	if team.IsStatus(err, http.StatusForbidden) {
		if st.Roster != nil && !st.Removed {
			// The hub stops taking a member's requests with the roster that
			// drops them, so that roster is out of reach: take its word.
			t.log.Warn("team: the hub refuses this member; removed from the team?", "team", t.id)
			st.Removed = true
		}
		return false, nil // waiting, or removed
	}
	if err != nil {
		return false, err
	}
	if st.Removed {
		t.log.Info("team: the hub takes this member's requests again", "team", t.id)
		st.Removed = false
	}
	if st.Roster == nil {
		anchor, err := team.Anchor(rosters, t.key.ID, st.Inviter)
		if err != nil {
			if errors.Is(err, team.ErrVerify) && len(rosters) > 0 && listsMe(rosters, t.key.ID) {
				t.log.Warn("team: the roster that lists this member is not signed by the inviting admin", "team", t.id, "err", err)
				return false, err
			}
			return false, nil // still waiting
		}
		st.Roster = anchor
		t.log.Info("team: joined", "team", t.id, "seq", anchor.Seq)
	}
	prevEpoch := st.Roster.Epoch
	head, err := team.Advance(st.Roster, rosters)
	if head != st.Roster {
		st.Roster = head
	}
	if err != nil {
		t.log.Warn("team: a roster from the hub does not verify; kept the last good one", "team", t.id, "seq", head.Seq, "err", err)
		return false, err
	}
	if _, ok := head.Member(t.key.ID); !ok {
		st.Removed = true
		return false, nil
	}
	if err := t.syncKey(ctx); err != nil {
		return false, err
	}
	t.membersLeft()
	return head.Epoch != prevEpoch, nil
}

func listsMe(rosters []*team.Roster, me string) bool {
	for _, r := range rosters {
		if _, ok := r.Member(me); ok {
			return true
		}
	}
	return false
}

// syncKey fetches and opens the team key of the roster's epoch unless it
// is held already. The caller holds t.op.
func (t *teamRT) syncKey(ctx context.Context) error {
	st := t.st
	if st.current() != nil {
		return nil
	}
	sks, err := t.client.Keys(ctx)
	if err != nil {
		return err
	}
	for _, sk := range sks {
		if sk.Epoch != st.Roster.Epoch {
			continue
		}
		K, err := t.key.OpenTeamKey(sk, st.Roster)
		if err != nil {
			t.log.Warn("team: a sealed team key from the hub does not open or match the roster", "team", t.id, "epoch", sk.Epoch, "err", err)
			continue
		}
		st.addKey(sk.Epoch, K)
		return nil
	}
	return errorf(Conflict, "no team key for epoch %d yet", st.Roster.Epoch)
}

// membersLeft forgets what belonged to members no longer in the roster:
// the hub dropped their bundles. Installed skills stay, marked as from a
// publisher who left. The caller holds t.op.
func (t *teamRT) membersLeft() {
	r := t.st.Roster
	for id, b := range t.st.Bundles {
		if _, ok := r.Member(b.Owner); ok || b.Owner == "" {
			continue
		}
		if b.Pending != nil {
			t.dropPending(id, b)
		}
		b.Head, b.Meta, b.Problem = nil, nil, ""
	}
	t.st.Subs = slices.DeleteFunc(t.st.Subs, func(s sub) bool {
		_, ok := r.Member(s.Owner)
		return !ok
	})
}
