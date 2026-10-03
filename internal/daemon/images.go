package daemon

// Images sent to an agent: saved in its state dir, then their paths are
// pasted into the prompt. Claude Code and Codex turn a pasted path to an
// image file into an image attachment ("[Image #1]").

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	fleetv1 "fleet/gen/fleetv1"
)

const (
	// MaxImageSize caps one image; an AttachImageRequest must fit a frame.
	MaxImageSize = 3584 << 10
	// imagesDir is the subdirectory of an agent's state dir for images.
	imagesDir = "images"
	// pasteSettle bounds waiting for the agent to show a pasted image, and
	// pastePoll is how often its screen is looked at meanwhile.
	pasteSettle = 3 * time.Second
	pastePoll   = 40 * time.Millisecond
)

// imageExt maps the image types agents accept to file extensions.
var imageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// saveImage checks data is an image an agent can read and saves it in the
// agent's state dir (mounted into its sandbox, if any), returning the path.
func saveImage(stateDir string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errf(codeInvalid, "empty image")
	}
	if len(data) > MaxImageSize {
		return "", errf(codeInvalid, "image too large (%.1f MiB, at most %.1f MiB)",
			float64(len(data))/(1<<20), float64(MaxImageSize)/(1<<20))
	}
	ext, ok := imageExt[http.DetectContentType(data)]
	if !ok {
		return "", errf(codeInvalid, "not a PNG, JPEG, GIF or WebP image")
	}
	dir := filepath.Join(stateDir, imagesDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "image-"+strconv.FormatInt(time.Now().UnixMilli(), 10)+"-"+hex.EncodeToString(b[:])+ext)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// attachImages saves images for a live agent and pastes their paths into
// its terminal, one paste each (a TUI recognises a paste that is exactly
// one image path). With guard it refuses while the agent shows a dialog,
// where the paste would not reach the prompt.
func (m *manager) attachImages(ctx context.Context, ref string, images [][]byte, guard bool) ([]string, error) {
	if len(images) == 0 {
		return nil, nil
	}
	m.mu.Lock()
	a, err := m.find(ref)
	var session, adapterID, stateDir string
	if err == nil {
		if !a.live() {
			err = errf(codeInvalid, "agent %s is not running", a.ID)
		}
		session, adapterID, stateDir = a.TmuxSession, a.Adapter, a.stateDir()
	}
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if guard && m.dialogOpen(ctx, session, adapterID) {
		return nil, errf(codeInvalid, "the agent is showing a dialog; answer it before sending images")
	}
	paths := make([]string, 0, len(images))
	for _, data := range images {
		p, err := saveImage(stateDir, data)
		if err != nil {
			return paths, err
		}
		if err := m.pasteSettled(ctx, session, p); err != nil {
			return paths, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// pasteSettled pastes text and waits until the screen has changed and
// then held still, or pasteSettle passed. Claude Code reads a pasted image
// in the background: what is typed meanwhile lands before its [Image #n].
func (m *manager) pasteSettled(ctx context.Context, session, text string) error {
	before, _ := m.d.tmux.Screen(ctx, session)
	if err := m.d.tmux.Paste(ctx, session, text); err != nil {
		return err
	}
	deadline := time.Now().Add(pasteSettle)
	last := before
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pastePoll):
		}
		s, err := m.d.tmux.Screen(ctx, session)
		if err != nil {
			return nil
		}
		if s != before && s == last {
			return nil
		}
		last = s
	}
	return nil
}

// attachImage serves AttachImageRequest.
func (m *manager) attachImage(ctx context.Context, req *fleetv1.AttachImageRequest) (*fleetv1.AttachImageResponse, error) {
	paths, err := m.attachImages(ctx, req.GetAgent(), [][]byte{req.GetData()}, false)
	if err != nil {
		return nil, err
	}
	return &fleetv1.AttachImageResponse{Path: paths[0]}, nil
}
