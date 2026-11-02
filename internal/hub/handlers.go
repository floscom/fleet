package hub

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"fleet/internal/team"
)

// ---------------------------------------------------------------------------
// Teams

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(s.clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	if !s.opts.Open {
		code, ok := strings.CutPrefix(r.Header.Get("Authorization"), team.CreateScheme)
		if !ok || !s.codes.redeem(code) {
			writeError(w, http.StatusUnauthorized, "a creation code from the hub operator is required (fleet hub code)")
			return
		}
	}
	body, err := readBody(r, maxJSONBody)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request too large")
		return
	}
	var req team.CreateRequest
	if err := readJSON(body, &req); err != nil || req.Roster == nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := team.VerifyFounding(req.Roster); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.teams[req.Roster.Team]; exists {
		writeError(w, http.StatusConflict, "team id taken")
		return
	}
	t := newTeamState(req.Roster.Team, s.opts.now())
	t.Rosters = []*team.Roster{req.Roster}
	if msg := t.setKeys(req.Keys); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	t.event(team.Event{Kind: team.EventRoster, Ref: "1"}, s.opts.now())
	if err := s.store.saveTeam(t); err != nil {
		s.fail(w, "save team", err)
		return
	}
	s.teams[t.ID] = t
	s.log.Info("team created", "team", t.ID)
	w.WriteHeader(http.StatusCreated)
}

// ---------------------------------------------------------------------------
// Rosters and keys

func (s *Server) handleRosters(w http.ResponseWriter, r *http.Request, a *authed) {
	since := queryUint(r, "since")
	var out []*team.Roster
	for _, ro := range a.team.Rosters {
		if ro.Seq > since {
			out = append(out, ro)
		}
	}
	writeJSON(w, http.StatusOK, team.RosterList{Rosters: out})
}

func (s *Server) handlePostRoster(w http.ResponseWriter, r *http.Request, a *authed) {
	var req team.RosterPost
	if err := readJSON(a.body, &req); err != nil || req.Roster == nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	t := a.team
	if req.Roster.Signer != a.member.ID {
		writeError(w, http.StatusForbidden, "the roster must be signed by the requesting admin")
		return
	}
	if err := team.VerifyNext(t.head(), req.Roster); err != nil {
		if req.Roster.Seq <= t.head().Seq {
			writeError(w, http.StatusConflict, "roster seq is behind: fetch the chain and try again")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	prev := t.head()
	existing := t.Keys
	if req.Roster.Epoch != prev.Epoch {
		existing = nil // keys of the old epoch are of no use to anyone now
	}
	keys, msg := mergeKeys(existing, req.Keys, t.ID, req.Roster)
	if msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	t.Rosters = append(t.Rosters, req.Roster)
	t.Keys = keys
	now := s.opts.now()
	// Members gone from the roster are cut off; members (re)added are let
	// back in and their join requests are done with.
	for _, m := range prev.Members {
		if _, ok := req.Roster.Member(m.ID); !ok {
			t.revoke(s.store, m.ID, now)
		}
	}
	for _, m := range req.Roster.Members {
		delete(t.Revoked, m.ID)
		delete(t.Joins, m.ID)
	}
	t.gcBlobs(s.store, now)
	t.event(team.Event{Kind: team.EventRoster, Ref: strconv.FormatUint(req.Roster.Seq, 10)}, now)
	for _, k := range req.Keys {
		t.event(team.Event{Kind: team.EventKeys, Member: k.Member}, now)
	}
	if err := s.store.saveTeam(t); err != nil {
		s.fail(w, "save team", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setKeys stores sealed keys for the current epoch, replacing a member's
// existing one. It returns a message when a key is not for this team,
// epoch or a member.
func (t *teamState) setKeys(keys []team.SealedKey) string {
	merged, msg := mergeKeys(t.Keys, keys, t.ID, t.head())
	if msg == "" {
		t.Keys = merged
	}
	return msg
}

// mergeKeys is setKeys without the side effect: existing with keys merged
// in, checked against roster.
func mergeKeys(existing, keys []team.SealedKey, teamID string, roster *team.Roster) ([]team.SealedKey, string) {
	out := slices.Clone(existing)
	for _, k := range keys {
		if k.Team != teamID || k.Epoch != roster.Epoch {
			return nil, "a sealed key is for another team or epoch"
		}
		if _, ok := roster.Member(k.Member); !ok {
			return nil, "a sealed key is for a non-member"
		}
		if len(k.Sealed) == 0 || len(k.Sealed) > 4096 {
			return nil, "a sealed key has a bad size"
		}
		out = slices.DeleteFunc(out, func(e team.SealedKey) bool { return e.Member == k.Member })
		out = append(out, k)
	}
	return out, ""
}

// revoke cuts a member off: no more requests, bundles and uploads dropped,
// no longer listed as having left.
func (t *teamState) revoke(st *store, id string, now time.Time) {
	t.Revoked[id] = now
	t.Left = slices.DeleteFunc(t.Left, func(l string) bool { return l == id })
	for b, h := range t.Heads {
		if h.Owner == id {
			delete(t.Heads, b)
		}
	}
	for ref, b := range t.Blobs {
		if b.By == id {
			t.dropBlob(st, ref)
		}
	}
	t.Keys = slices.DeleteFunc(t.Keys, func(k team.SealedKey) bool { return k.Member == id })
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request, a *authed) {
	out := []team.SealedKey{}
	for _, k := range a.team.Keys {
		if k.Member == a.member.ID {
			out = append(out, k)
		}
	}
	writeJSON(w, http.StatusOK, team.KeyList{Keys: out})
}

func (s *Server) handlePostKeys(w http.ResponseWriter, r *http.Request, a *authed) {
	var req team.KeyList
	if err := readJSON(a.body, &req); err != nil || len(req.Keys) == 0 {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	if msg := a.team.setKeys(req.Keys); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	now := s.opts.now()
	for _, k := range req.Keys {
		a.team.event(team.Event{Kind: team.EventKeys, Member: k.Member}, now)
	}
	if err := s.store.saveTeam(a.team); err != nil {
		s.fail(w, "save team", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Invites, joins, leaving

func (s *Server) handlePostInvite(w http.ResponseWriter, r *http.Request, a *authed) {
	var req team.InviteRegistration
	if err := readJSON(a.body, &req); err != nil || len(req.ID) != 64 || !isLowerHex(req.ID) {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	now := s.opts.now()
	if !req.Expires.After(now) || req.Expires.After(now.Add(maxInviteTTL)) {
		writeError(w, http.StatusUnprocessableEntity, "expiry must be in the future and within 30 days")
		return
	}
	t := a.team
	t.pruneInvites(now)
	if len(t.Invites) >= maxInvites {
		writeError(w, http.StatusTooManyRequests, "too many open invites")
		return
	}
	t.Invites[req.ID] = inviteRec{Expires: req.Expires}
	if err := s.store.saveTeam(t); err != nil {
		s.fail(w, "save team", err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// pruneInvites drops expired and used invites.
func (t *teamState) pruneInvites(now time.Time) {
	for id, inv := range t.Invites {
		if inv.Used || !now.Before(inv.Expires) {
			delete(t.Invites, id)
		}
	}
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(s.clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	credB64, ok := strings.CutPrefix(r.Header.Get("Authorization"), team.InviteAuthScheme)
	cred, ok2 := team.ParseCredential(credB64)
	if !ok || !ok2 {
		writeError(w, http.StatusUnauthorized, "an invite is required")
		return
	}
	body, err := readBody(r, maxJSONBody)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request too large")
		return
	}
	var req team.JoinRequest
	if err := readJSON(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	m, err := team.MemberFromKeys(req.SignPub, req.KEMPub)
	if err != nil || len(req.Proof) != 32 || len(req.NameSealed) == 0 || len(req.NameSealed) > maxNameSealed {
		writeError(w, http.StatusBadRequest, "bad keys, proof or name")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.teams[r.PathValue("t")]
	inviteID := team.InviteID(cred)
	now := s.opts.now()
	var inv inviteRec
	if ok {
		inv, ok = t.Invites[inviteID]
	}
	if !ok || inv.Used || !now.Before(inv.Expires) {
		writeError(w, http.StatusForbidden, "the invite is unknown, used or expired")
		return
	}
	if _, isMember := t.head().Member(m.ID); isMember {
		writeError(w, http.StatusConflict, "already a member")
		return
	}
	if len(t.Joins) >= maxJoins {
		writeError(w, http.StatusTooManyRequests, "too many pending join requests")
		return
	}
	// Single use, from here on: a leaked invite that has been used is
	// harmless.
	inv.Used = true
	t.Invites[inviteID] = inv
	req.ID, req.Invite, req.Created = m.ID, inviteID, now
	t.Joins[m.ID] = req
	t.event(team.Event{Kind: team.EventJoin, Ref: m.ID}, now)
	if err := s.store.saveTeam(t); err != nil {
		s.fail(w, "save team", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleJoins(w http.ResponseWriter, r *http.Request, a *authed) {
	out := team.JoinList{Joins: []team.JoinRequest{}, Left: []string{}}
	for _, j := range a.team.Joins {
		out.Joins = append(out.Joins, j)
	}
	sort.Slice(out.Joins, func(i, j int) bool { return out.Joins[i].Created.Before(out.Joins[j].Created) })
	out.Left = append(out.Left, a.team.Left...)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteJoin(w http.ResponseWriter, r *http.Request, a *authed) {
	id := r.PathValue("id")
	if _, ok := a.team.Joins[id]; !ok {
		writeError(w, http.StatusNotFound, "no such join request")
		return
	}
	delete(a.team.Joins, id)
	if err := s.store.saveTeam(a.team); err != nil {
		s.fail(w, "save team", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLeave(w http.ResponseWriter, r *http.Request, a *authed) {
	t, now := a.team, s.opts.now()
	if a.member.Admin && len(t.head().Admins()) == 1 {
		writeError(w, http.StatusConflict, "the last admin cannot leave: make someone else an admin first")
		return
	}
	t.revoke(s.store, a.member.ID, now)
	t.Left = append(t.Left, a.member.ID)
	t.event(team.Event{Kind: team.EventLeft, Ref: a.member.ID}, now)
	if err := s.store.saveTeam(t); err != nil {
		s.fail(w, "save team", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Bundles

func (s *Server) handleHeads(w http.ResponseWriter, r *http.Request, a *authed) {
	out := make([]*team.Head, 0, len(a.team.Heads))
	for _, h := range a.team.Heads {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, team.HeadList{Heads: out})
}

func (s *Server) handleHead(w http.ResponseWriter, r *http.Request, a *authed) {
	h, ok := a.team.Heads[r.PathValue("b")]
	if !ok {
		writeError(w, http.StatusNotFound, "no such bundle")
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) handlePutHead(w http.ResponseWriter, r *http.Request, a *authed) {
	var h team.Head
	if err := readJSON(a.body, &h); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	t := a.team
	if h.ID != r.PathValue("b") || h.Team != t.ID {
		writeError(w, http.StatusBadRequest, "head does not match the route")
		return
	}
	if h.Owner != a.member.ID {
		writeError(w, http.StatusForbidden, "only the owner publishes a bundle")
		return
	}
	if err := team.VerifyHead(&h, t.head()); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if prev, ok := t.Heads[h.ID]; ok {
		if prev.Owner != h.Owner {
			writeError(w, http.StatusForbidden, "that bundle belongs to another member")
			return
		}
		if h.Version <= prev.Version {
			writeError(w, http.StatusConflict, "version must rise")
			return
		}
	}
	if !h.Deleted {
		b, ok := t.Blobs[h.DataRef]
		if !ok || b.Size != h.DataSize {
			writeError(w, http.StatusUnprocessableEntity, "upload the data first")
			return
		}
	}
	now := s.opts.now()
	prev := t.Heads[h.ID]
	t.Heads[h.ID] = &h
	if prev != nil && !prev.Deleted && prev.DataRef != h.DataRef {
		t.dropBlob(s.store, prev.DataRef) // superseded: nobody needs it now
	}
	t.event(team.Event{Kind: team.EventBundle, Ref: h.ID}, now)
	if err := s.store.saveTeam(t); err != nil {
		s.fail(w, "save team", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePutData(w http.ResponseWriter, r *http.Request, a *authed) {
	ref, t := r.PathValue("ref"), a.team
	if len(ref) != 64 || !isLowerHex(ref) || team.DataRef(a.body) != ref {
		writeError(w, http.StatusBadRequest, "data does not match its ref")
		return
	}
	if len(a.body) == 0 {
		writeError(w, http.StatusBadRequest, "empty data")
		return
	}
	if _, ok := t.Blobs[ref]; ok {
		w.WriteHeader(http.StatusNoContent) // the same bytes are here already
		return
	}
	now := s.opts.now()
	t.gcBlobs(s.store, now)
	if t.used()+int64(len(a.body)) > s.opts.Quota {
		writeError(w, http.StatusInsufficientStorage, "the team's storage quota is used up")
		return
	}
	if err := s.store.putBlob(t.ID, ref, a.body); err != nil {
		s.fail(w, "store data", err)
		return
	}
	t.Blobs[ref] = blobRec{Size: int64(len(a.body)), Uploaded: now, By: a.member.ID}
	if err := s.store.saveTeam(t); err != nil {
		s.fail(w, "save team", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleData(w http.ResponseWriter, r *http.Request, a *authed) {
	ref := r.PathValue("ref")
	// Only data a live head vouches for is served: an upload nobody has
	// published is nobody's business.
	if _, ok := a.team.Blobs[ref]; !ok || !a.team.referenced()[ref] {
		writeError(w, http.StatusNotFound, "no such data")
		return
	}
	data, err := s.store.getBlob(a.team.ID, ref)
	if errors.Is(err, os.ErrNotExist) {
		s.log.Error("blob missing", "team", a.team.ID, "ref", ref)
		writeError(w, http.StatusNotFound, "no such data")
		return
	}
	if err != nil {
		s.fail(w, "read data", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// gcBlobs deletes blobs no live head references once they have waited
// pendingTTL for one (an upload whose head never came).
func (t *teamState) gcBlobs(st *store, now time.Time) {
	for ref, b := range t.Blobs {
		if now.Sub(b.Uploaded) >= pendingTTL {
			t.dropBlob(st, ref)
		}
	}
}

// dropBlob deletes a blob unless a live head still references it.
func (t *teamState) dropBlob(st *store, ref string) {
	if t.referenced()[ref] {
		return
	}
	if err := st.deleteBlob(t.ID, ref); err == nil {
		delete(t.Blobs, ref)
	}
}

// ---------------------------------------------------------------------------
// Helpers

// readBody reads an unauthenticated request's body, capped.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, limit))
	return buf.Bytes(), err
}

func queryUint(r *http.Request, name string) uint64 {
	v, _ := strconv.ParseUint(r.URL.Query().Get(name), 10, 64)
	return v
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// fail answers a storage error without leaking paths.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("hub: "+what, "err", err)
	writeError(w, http.StatusInternalServerError, what+" failed")
}
