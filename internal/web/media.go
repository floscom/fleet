package web

// The images of a session, for its Media tab:
//
//	GET /api/agents/{id}/media   the images of its conversation (see serveMedia)

import (
	"errors"
	"io/fs"
	"net/http"
	"strconv"

	"fleet/internal/transcript"
)

// mediaScan bounds the transcript bytes one media request reads; a longer
// transcript is read over several requests (mediaReply.More).
const mediaScan = 64 << 20

// Media is an image of a conversation: read it with GET .../image
// (file, line, n).
type Media struct {
	transcript.Image
	// TimeMs is when it was sent or returned, ms since the epoch; 0 if unknown.
	TimeMs int64 `json:"ts,omitempty"`
	// From is who brought it in: "user" (sent with a message) or "tool"
	// (returned by a tool call: a file read, a screenshot).
	From string `json:"from"`
	// Tool and Text are the call that returned it, its name and one-line
	// summary (e.g. the path read); empty if unknown.
	Tool string `json:"tool,omitempty"`
	Text string `json:"text,omitempty"`
}

// mediaReply is the answer of GET /api/agents/{id}/media.
type mediaReply struct {
	// File names the transcript the offsets are in, "" while there is none.
	File  string  `json:"file"`
	Media []Media `json:"media"`
	// End is the offset after what was read: pass it as "after" for newer
	// images.
	End int64 `json:"end"`
	// Reset means Media replace what the page holds: the first read, or
	// the transcript changed.
	Reset bool `json:"reset"`
	// More means the transcript was not read to its end: ask again right away.
	More bool `json:"more"`
}

// apiMedia serves the images of an agent's conversation.
func (s *Server) apiMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.serveMedia(w, r, func() (Chat, error) { return s.opts.Source.Chat(id) })
}

// serveMedia lists the images in the transcript find returns, oldest
// first: from its start, or with file=F&after=N those after offset N of
// file F. A file that is not the agent's transcript (anymore) is read
// from its start, with Reset.
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, find func() (Chat, error)) {
	q := r.URL.Query()
	c, err := find()
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	reply := mediaReply{File: fileKey(c.Path), Media: []Media{}}
	if c.Path == "" || c.Image == nil {
		reply.Reset = q.Get("file") != ""
		writeJSON(w, http.StatusOK, reply)
		return
	}
	from, err := strconv.ParseInt(q.Get("after"), 10, 64)
	if err != nil || from < 0 || q.Get("file") != reply.File {
		from, reply.Reset = 0, true
	}
	err = reply.scan(c, from)
	if errors.Is(err, transcript.ErrShrunk) {
		reply = mediaReply{File: reply.File, Media: []Media{}, Reset: true}
		err = reply.scan(c, 0)
	}
	if errors.Is(err, fs.ErrNotExist) {
		reply = mediaReply{Media: []Media{}, Reset: q.Get("file") != ""}
		err = nil
	}
	if err != nil {
		s.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

// scan adds the images of the transcript from offset from on, up to about
// mediaScan bytes.
func (m *mediaReply) scan(c Chat, from int64) error {
	calls := map[string]transcript.Entry{} // tool calls by ID, for their results
	m.End = from
	for {
		p, err := transcript.Forward(c.Path, m.End, c.Parse)
		if err != nil {
			return err
		}
		m.End, m.More = p.End, p.More
		for _, e := range p.Entries {
			if e.Kind == transcript.Tool && e.ID != "" {
				calls[e.ID] = e
			}
			if len(e.Images) == 0 {
				continue
			}
			md := Media{TimeMs: e.TimeMs, From: "tool"}
			switch e.Kind {
			case transcript.User:
				md.From = "user"
			case transcript.Result:
				if call, ok := calls[e.ID]; ok {
					md.Tool, md.Text = call.Name, call.Text
				}
			default:
				md.Tool, md.Text = e.Name, e.Text
			}
			for _, im := range e.Images {
				md.Image = im
				m.Media = append(m.Media, md)
			}
		}
		if !p.More || m.End-from >= mediaScan {
			return nil
		}
	}
}
