package web

// Sending a file of a session straight to another fleet daemon, found the
// same way as the machines on the dashboard's Devices view (peers.go):
// the dashboard asks this daemon to push one of a session's files (see
// files.go, fileOf) to a peer's upload route, signed with the fleet key
// like any other peer request. The file goes daemon to daemon; the
// browser only names it and picks the destination.
//
//	POST /api/agents/{id}/send   {"path": "...", "host": "<peer id>"}
//	  -> {"host", "name", "path", "size"}: the peer's id and name, and
//	     where it kept the file.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

// apiSendFile streams a file of an agent's session to another fleet
// daemon's /api/uploads, reusing the same signing and forwarding as a
// browser's request to /api/hosts/{id}/... (peers.go).
func (s *Server) apiSendFile(w http.ResponseWriter, r *http.Request) {
	if s.opts.KeyPath == "" {
		writeError(w, http.StatusNotFound, "this fleet does not use a fleet key: run `fleet web` to create one")
		return
	}
	var req struct {
		Path string `json:"path"`
		Host string `json:"host"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	p := filepath.Clean(req.Path)
	if !filepath.IsAbs(p) {
		writeError(w, http.StatusBadRequest, "path must be absolute")
		return
	}
	if req.Host == "" || req.Host == s.opts.Source.Info().ID {
		writeError(w, http.StatusBadRequest, "pick another machine to send the file to")
		return
	}
	c, err := s.opts.Source.Chat(r.PathValue("id"))
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	ok, err := s.fileOf(c, p)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not a file of this session")
		return
	}
	peer, ok := s.hub.peer(req.Host)
	if !ok {
		writeError(w, http.StatusNotFound, "that machine is not on the network (anymore)")
		return
	}
	name := peer.Name
	if name == "" {
		name = peer.Host
	}
	if peer.WebPort == 0 || len(peer.Addrs) == 0 {
		writeError(w, http.StatusConflict, name+" has no dashboard on the network (web is off or loopback-only there)")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	if !fi.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "not a regular file")
		return
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		s.writeSourceError(w, err)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		s.writeSourceError(w, err)
		return
	}
	key, err := LoadOrCreateToken(s.opts.KeyPath)
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	uri := "/api/uploads?name=" + url.QueryEscape(filepath.Base(p))
	ctx, cancel := context.WithTimeout(r.Context(), uploadTimeout)
	defer cancel()
	body := outBody{r: f, size: fi.Size(), sum: h.Sum(nil), stream: true}
	status, reply, err := s.forwardBody(ctx, peer, key, http.MethodPost, uri, body)
	switch {
	case errors.Is(err, errNotJoined) || status == http.StatusUnauthorized:
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":     name + " is not in this fleet yet: run `fleet start --join <key>` there",
			"notJoined": true,
		})
	case err != nil:
		s.opts.Log.Debug("web ui: send file failed", "peer", name, "err", err)
		writeError(w, http.StatusBadGateway, "cannot reach "+name)
	case status != http.StatusCreated:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(reply)
	default:
		var up struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		}
		if err := json.Unmarshal(reply, &up); err != nil {
			writeError(w, http.StatusBadGateway, "bad reply from "+name)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"host": req.Host, "name": name, "path": up.Path, "size": up.Size})
	}
}
