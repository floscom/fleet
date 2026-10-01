package claude

import (
	"encoding/base64"
	"encoding/json"
)

// imageSource is where an image block's data is. Claude Code records
// images inline, base64 encoded: pasted into a message, or returned by a
// tool (Read of an image file, screenshots of MCP tools).
type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
}

func (s imageSource) inline() bool { return s.Type == "base64" }

// TranscriptImage returns image n of a transcript line, numbered in the
// order userContent meets them: the message's image blocks, and those of
// its tool results (adapter.TranscriptImager).
func (c *claude) TranscriptImage(line []byte, n int) (string, []byte, bool) {
	return lineImage(line, n)
}

func lineImage(line []byte, n int) (string, []byte, bool) {
	type imageBlock struct {
		Type   string `json:"type"`
		Source struct {
			imageSource
			Data string `json:"data"`
		} `json:"source"`
		Content json.RawMessage `json:"content"`
	}
	var r struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	var blocks []imageBlock
	if !decode(line, &r) || json.Unmarshal(r.Message.Content, &blocks) != nil {
		return "", nil, false
	}
	i := 0
	found := func(b imageBlock) ([]byte, bool) {
		if b.Type != "image" || !b.Source.inline() {
			return nil, false
		}
		if i++; i-1 != n {
			return nil, false
		}
		data, err := base64.StdEncoding.DecodeString(b.Source.Data)
		return data, err == nil
	}
	for _, b := range blocks {
		if data, ok := found(b); ok {
			return b.Source.MediaType, data, true
		}
		if b.Type != "tool_result" {
			continue
		}
		var inner []imageBlock
		if json.Unmarshal(b.Content, &inner) != nil {
			continue
		}
		for _, ib := range inner {
			if data, ok := found(ib); ok {
				return ib.Source.MediaType, data, true
			}
		}
	}
	return "", nil, false
}
