package web

// Keeping the shared skills (package skills) the same on every daemon of
// the fleet. Each daemon pulls: now and then, and right after another one
// says its skills changed, it asks every peer for its manifest and fetches
// whatever skill a peer has a newer version of. A machine that was away
// catches up on its next round, and one that cannot reach the daemon a
// change came from still gets it from one that has it by now.
//
//	GET  /api/skills          -> {"skills": [skills.Entry...]}: the manifest
//	GET  /api/skills/{name}   -> {"skill": skills.Entry, "data": base64 tar.gz}
//	POST /api/skills/sync     -> {}: my skills changed, pull now
//
// All three take requests signed with the fleet key, like any peer request.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"fleet/internal/skills"
)

const (
	// skillPoll is how often skills are pulled from every peer.
	skillPoll = time.Minute
	// skillStart is the first round's delay, for the first browse to
	// find the peers.
	skillStart = 10 * time.Second
	// skillTimeout bounds fetching one skill.
	skillTimeout = 2 * time.Minute
)

func (s *Server) apiSkills(w http.ResponseWriter, r *http.Request) {
	if s.opts.Skills == nil {
		writeError(w, http.StatusNotFound, "this fleet shares no skills")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": s.opts.Skills.Manifest()})
}

func (s *Server) apiSkill(w http.ResponseWriter, r *http.Request) {
	if s.opts.Skills == nil {
		writeError(w, http.StatusNotFound, "this fleet shares no skills")
		return
	}
	e, data, err := s.opts.Skills.Archive(r.PathValue("name"))
	switch {
	case errors.Is(err, skills.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such skill")
	case errors.Is(err, skills.ErrChanging):
		writeError(w, http.StatusConflict, "the skill is changing, try again")
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"skill": e, "data": data})
	}
}

func (s *Server) apiSkillSync(w http.ResponseWriter, r *http.Request) {
	if s.opts.Skills == nil {
		writeError(w, http.StatusNotFound, "this fleet shares no skills")
		return
	}
	select {
	case s.skillKick <- struct{}{}:
	default:
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// skillLoop pulls skills from the peers and tells them about changes here
// until ctx ends.
func (s *Server) skillLoop(ctx context.Context) {
	t := time.NewTimer(skillStart)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.skillKick:
		case <-s.opts.Skills.Changed():
			s.pokeSkillPeers(ctx)
			continue
		}
		s.pullSkills(ctx)
		t.Reset(skillPoll)
	}
}

// skillPeers are the peers to share skills with, and the fleet key; none
// without a key.
func (s *Server) skillPeers() ([]Peer, string) {
	key, err := readToken(s.opts.KeyPath)
	if err != nil || key == "" {
		return nil, ""
	}
	s.hub.mu.Lock()
	all := s.hub.peerListLocked()
	s.hub.mu.Unlock()
	var out []Peer
	for _, p := range all {
		if !p.Self && p.ID != "" && p.WebPort != 0 && len(p.Addrs) > 0 {
			out = append(out, p)
		}
	}
	return out, key
}

// pokeSkillPeers tells every peer that the skills here changed.
func (s *Server) pokeSkillPeers(ctx context.Context) {
	peers, key := s.skillPeers()
	for _, p := range peers {
		cctx, cancel := context.WithTimeout(ctx, peerTimeout)
		if _, _, err := s.forward(cctx, p, key, http.MethodPost, "/api/skills/sync", nil); err != nil {
			s.opts.Log.Debug("skills: telling a peer failed", "peer", p.Name, "err", err)
		}
		cancel()
	}
}

// pullSkills fetches from every peer the skills it has newer versions of.
func (s *Server) pullSkills(ctx context.Context) {
	peers, key := s.skillPeers()
	for _, p := range peers {
		s.pullSkillsFrom(ctx, p, key)
	}
}

func (s *Server) pullSkillsFrom(ctx context.Context, p Peer, key string) {
	st := s.opts.Skills
	cctx, cancel := context.WithTimeout(ctx, peerTimeout)
	status, reply, err := s.forward(cctx, p, key, http.MethodGet, "/api/skills", nil)
	cancel()
	if err != nil || status != http.StatusOK {
		// Not in this fleet, older than shared skills, or away.
		s.opts.Log.Debug("skills: asking a peer failed", "peer", p.Name, "status", status, "err", err)
		return
	}
	var m struct {
		Skills []skills.Entry `json:"skills"`
	}
	if err := json.Unmarshal(reply, &m); err != nil {
		return
	}
	for _, e := range st.Want(m.Skills) {
		var data []byte
		if st.Needs(e) {
			cctx, cancel := context.WithTimeout(ctx, skillTimeout)
			status, reply, err := s.forward(cctx, p, key, http.MethodGet, "/api/skills/"+url.PathEscape(e.Name), nil)
			cancel()
			var got struct {
				Skill skills.Entry `json:"skill"`
				Data  []byte       `json:"data"`
			}
			if err == nil && status == http.StatusOK {
				err = json.Unmarshal(reply, &got)
				if err == nil && got.Skill.Name != e.Name {
					err = errors.New("sent another skill")
				}
			} else if err == nil {
				err = errors.New(http.StatusText(status))
			}
			if err != nil {
				s.opts.Log.Debug("skills: fetching a skill failed", "peer", p.Name, "skill", e.Name, "err", err)
				continue
			}
			// What the peer has now, maybe newer than its manifest said.
			e, data = got.Skill, got.Data
		}
		applied, err := st.Apply(e, data)
		if err != nil {
			s.opts.Log.Warn("skills: taking a skill from a peer failed", "peer", p.Name, "skill", e.Name, "err", err)
		}
		if !applied {
			continue
		}
		verb := "updated"
		if e.Deleted {
			verb = "deleted"
		}
		s.opts.Log.Info("skills: skill "+verb+" from another machine", "skill", e.Name, "peer", p.Name, "by", e.Host)
	}
}
