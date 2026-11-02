package web

// Teams (docs/TEAMS.md): the admin API over this daemon's gateway
// (package gateway), for the dashboard and `fleet team`. A team lives on
// the daemon it was created or joined on, so these routes take the admin
// token only: requests signed with the fleet key do not reach them, and
// they are not forwarded to other daemons.
//
//	GET    /api/teams                          -> {"teams": [TeamView...]}
//	POST   /api/teams            {hub, code, name, me}      -> 201 {"team": TeamView}
//	POST   /api/teams/join       {invite, me}               -> 201 {"team": TeamView}
//	GET    /api/teams/{t}                      -> {"team": TeamView}
//	POST   /api/teams/{t}/sync                 -> {"team": TeamView}
//	POST   /api/teams/{t}/invites {ttl}        -> 201 {"invite", "expiresMs", "team"}
//	POST   /api/teams/{t}/members/{m}/admin {admin}         -> {"team": TeamView}
//	DELETE /api/teams/{t}/members/{m}          -> {"team": TeamView}
//	POST   /api/teams/{t}/leave                -> {}
//	POST   /api/teams/{t}/publish     {skill}  -> {"team": TeamView}
//	POST   /api/teams/{t}/unpublish   {skill}  -> {"team": TeamView}
//	POST   /api/teams/{t}/subscribe   {ref}    -> {"team": TeamView}
//	POST   /api/teams/{t}/unsubscribe {ref}    -> {"team": TeamView}
//	POST   /api/teams/{t}/pin         {ref, pin}            -> {"team": TeamView}
//	POST   /api/teams/{t}/apply       {bundle}              -> {"team": TeamView}
//	GET    /api/teams/{t}/diff/{bundle...}     -> {"diff": DiffView}
//
// {t} is a team id, name or id prefix; {m} a member name, id or id
// prefix; a ref "<member>/<skill>" or "<member>/*"; a bundle a bundle id,
// "<member>/<skill>" or the name a team skill is installed under.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"fleet/internal/gateway"
)

// teamTimeout bounds a team request: it may download a skill or two.
const teamTimeout = 2 * time.Minute

// teamRoutes registers the team routes on mux.
func (s *Server) teamRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/teams", s.apiTeams)
	mux.HandleFunc("POST /api/teams", s.apiCreateTeam)
	mux.HandleFunc("POST /api/teams/join", s.apiJoinTeam)
	mux.HandleFunc("GET /api/teams/{t}", s.apiTeam)
	mux.HandleFunc("POST /api/teams/{t}/sync", s.apiSyncTeam)
	mux.HandleFunc("POST /api/teams/{t}/invites", s.apiInvite)
	mux.HandleFunc("POST /api/teams/{t}/members/{m}/admin", s.apiTeamAdmin)
	mux.HandleFunc("DELETE /api/teams/{t}/members/{m}", s.apiRemoveMember)
	mux.HandleFunc("POST /api/teams/{t}/leave", s.apiLeaveTeam)
	mux.HandleFunc("POST /api/teams/{t}/publish", s.teamSkillOp((*gateway.Gateway).Publish))
	mux.HandleFunc("POST /api/teams/{t}/unpublish", s.teamSkillOp((*gateway.Gateway).Unpublish))
	mux.HandleFunc("POST /api/teams/{t}/subscribe", s.teamRefOp((*gateway.Gateway).Subscribe))
	mux.HandleFunc("POST /api/teams/{t}/unsubscribe", s.teamRefOp((*gateway.Gateway).Unsubscribe))
	mux.HandleFunc("POST /api/teams/{t}/pin", s.apiPin)
	mux.HandleFunc("POST /api/teams/{t}/apply", s.apiApply)
	mux.HandleFunc("GET /api/teams/{t}/diff/{bundle...}", s.apiDiff)
}

// teams returns the gateway, or answers 404 without one.
func (s *Server) teams(w http.ResponseWriter) *gateway.Gateway {
	if s.opts.Teams == nil {
		writeError(w, http.StatusNotFound, "teams are not available on this daemon")
	}
	return s.opts.Teams
}

func (s *Server) apiTeams(w http.ResponseWriter, r *http.Request) {
	if g := s.teams(w); g != nil {
		writeJSON(w, http.StatusOK, map[string][]gateway.TeamView{"teams": g.Teams()})
	}
}

func (s *Server) apiTeam(w http.ResponseWriter, r *http.Request) {
	if g := s.teams(w); g != nil {
		s.writeTeam(w, g, r.PathValue("t"), http.StatusOK, nil)
	}
}

func (s *Server) apiCreateTeam(w http.ResponseWriter, r *http.Request) {
	g := s.teams(w)
	var req struct {
		Hub  string `json:"hub"`
		Code string `json:"code"`
		Name string `json:"name"`
		Me   string `json:"me"`
	}
	if g == nil || !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), teamTimeout)
	defer cancel()
	v, err := g.Create(ctx, req.Hub, req.Code, req.Name, req.Me)
	if err != nil {
		s.writeTeamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]gateway.TeamView{"team": v})
}

func (s *Server) apiJoinTeam(w http.ResponseWriter, r *http.Request) {
	g := s.teams(w)
	var req struct {
		Invite string `json:"invite"`
		Me     string `json:"me"`
	}
	if g == nil || !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), teamTimeout)
	defer cancel()
	v, err := g.Join(ctx, req.Invite, req.Me)
	if err != nil {
		s.writeTeamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]gateway.TeamView{"team": v})
}

func (s *Server) apiSyncTeam(w http.ResponseWriter, r *http.Request) {
	g := s.teams(w)
	if g == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), teamTimeout)
	defer cancel()
	t := r.PathValue("t")
	s.writeTeam(w, g, t, http.StatusOK, g.Sync(ctx, t))
}

// apiInvite makes an invite: {"ttl": "7d"} (Go durations and days; empty
// for the default week).
func (s *Server) apiInvite(w http.ResponseWriter, r *http.Request) {
	g := s.teams(w)
	var req struct {
		TTL string `json:"ttl"`
	}
	if g == nil || !decodeOptional(w, r, &req) {
		return
	}
	ttl, err := gateway.ParseTTL(req.TTL)
	if err != nil {
		s.writeTeamError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), teamTimeout)
	defer cancel()
	t := r.PathValue("t")
	inv, expires, err := g.Invite(ctx, t, ttl)
	if err != nil {
		s.writeTeamError(w, err)
		return
	}
	v, _ := g.Team(t)
	writeJSON(w, http.StatusCreated, map[string]any{"invite": inv, "expiresMs": expires.UnixMilli(), "team": v})
}

func (s *Server) apiTeamAdmin(w http.ResponseWriter, r *http.Request) {
	g := s.teams(w)
	var req struct {
		Admin bool `json:"admin"`
	}
	if g == nil || !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), teamTimeout)
	defer cancel()
	t := r.PathValue("t")
	s.writeTeam(w, g, t, http.StatusOK, g.SetAdmin(ctx, t, r.PathValue("m"), req.Admin))
}

func (s *Server) apiRemoveMember(w http.ResponseWriter, r *http.Request) {
	g := s.teams(w)
	if g == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), teamTimeout)
	defer cancel()
	t := r.PathValue("t")
	s.writeTeam(w, g, t, http.StatusOK, g.Remove(ctx, t, r.PathValue("m")))
}

func (s *Server) apiLeaveTeam(w http.ResponseWriter, r *http.Request) {
	g := s.teams(w)
	if g == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), teamTimeout)
	defer cancel()
	if err := g.Leave(ctx, r.PathValue("t")); err != nil {
		s.writeTeamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// teamSkillOp serves an operation on one of the user's own skills:
// {"skill": name}.
func (s *Server) teamSkillOp(op func(*gateway.Gateway, context.Context, string, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g := s.teams(w)
		var req struct {
			Skill string `json:"skill"`
		}
		if g == nil || !decode(w, r, &req) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), teamTimeout)
		defer cancel()
		t := r.PathValue("t")
		s.writeTeam(w, g, t, http.StatusOK, op(g, ctx, t, req.Skill))
	}
}

// teamRefOp serves a subscription change: {"ref": "<member>/<skill>"}.
func (s *Server) teamRefOp(op func(*gateway.Gateway, context.Context, string, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g := s.teams(w)
		var req struct {
			Ref string `json:"ref"`
		}
		if g == nil || !decode(w, r, &req) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), teamTimeout)
		defer cancel()
		t := r.PathValue("t")
		s.writeTeam(w, g, t, http.StatusOK, op(g, ctx, t, req.Ref))
	}
}

func (s *Server) apiPin(w http.ResponseWriter, r *http.Request) {
	g := s.teams(w)
	var req struct {
		Ref string `json:"ref"`
		Pin bool   `json:"pin"`
	}
	if g == nil || !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), teamTimeout)
	defer cancel()
	t := r.PathValue("t")
	s.writeTeam(w, g, t, http.StatusOK, g.Pin(ctx, t, req.Ref, req.Pin))
}

func (s *Server) apiApply(w http.ResponseWriter, r *http.Request) {
	g := s.teams(w)
	var req struct {
		Bundle string `json:"bundle"`
	}
	if g == nil || !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), teamTimeout)
	defer cancel()
	t := r.PathValue("t")
	s.writeTeam(w, g, t, http.StatusOK, g.Apply(ctx, t, req.Bundle))
}

func (s *Server) apiDiff(w http.ResponseWriter, r *http.Request) {
	g := s.teams(w)
	if g == nil {
		return
	}
	d, err := g.Diff(r.PathValue("t"), r.PathValue("bundle"))
	if err != nil {
		s.writeTeamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]gateway.DiffView{"diff": d})
}

// writeTeam answers {"team": TeamView} for team t after an operation, or
// the operation's error.
func (s *Server) writeTeam(w http.ResponseWriter, g *gateway.Gateway, t string, status int, err error) {
	if err != nil {
		s.writeTeamError(w, err)
		return
	}
	v, err := g.Team(t)
	if err != nil {
		s.writeTeamError(w, err)
		return
	}
	writeJSON(w, status, map[string]gateway.TeamView{"team": v})
}

// writeTeamError sends a gateway error with the status of its kind, and
// hides anything else.
func (s *Server) writeTeamError(w http.ResponseWriter, err error) {
	var e *gateway.Error
	if !errors.As(err, &e) {
		s.opts.Log.Warn("teams: request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	status := map[gateway.Kind]int{
		gateway.Invalid:   http.StatusBadRequest,
		gateway.NotFound:  http.StatusNotFound,
		gateway.Forbidden: http.StatusForbidden,
		gateway.Conflict:  http.StatusConflict,
		gateway.Upstream:  http.StatusBadGateway,
	}[e.Kind]
	if status == 0 {
		status = http.StatusInternalServerError
	}
	writeError(w, status, e.Msg)
}

// decodeOptional is decode for a body that may be empty.
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 {
		return true
	}
	return decode(w, r, v)
}
