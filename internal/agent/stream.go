package agent

import (
	"encoding/json"
	"strings"
)

// Stream-json is a different output format from print mode, not a variation of
// it: instead of one result object at the end, the agent emits one JSON object
// per line as it goes, and the last of them is the result. Everything before it
// is progress.
//
// Only two kinds of line matter here. A text delta is the agent writing its
// answer, which is what makes a live interface possible. The final result is
// what print mode would have returned, and is parsed by exactly the same code,
// so there is one definition of what a run cost rather than two.

// streamLine is the envelope every line shares.
type streamLine struct {
	Type string `json:"type"`
	// Event carries the model's own stream events, where the text lives.
	Event struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	// Message is present on assistant turns, whose content blocks carry text
	// for callers that did not ask for partial messages.
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// streamText extracts whatever text a line represents, or "" when the line is
// not the agent writing.
//
// Both shapes are read because they are not alternatives: partial messages
// give deltas while the agent is writing, and the assistant line that follows
// repeats the finished block. Taking only deltas loses the output of a run
// whose version did not emit them; taking both would duplicate it, so the
// caller suppresses the repeat.
func streamText(line []byte) (text string, final bool) {
	var parsed streamLine
	if err := json.Unmarshal(line, &parsed); err != nil {
		return "", false
	}
	switch parsed.Type {
	case "stream_event":
		if parsed.Event.Type == "content_block_delta" && parsed.Event.Delta.Type == "text_delta" {
			return parsed.Event.Delta.Text, false
		}
	case "assistant":
		var b strings.Builder
		for _, block := range parsed.Message.Content {
			if block.Type == "text" {
				b.WriteString(block.Text)
			}
		}
		return b.String(), true
	}
	return "", false
}

// streamReader turns a sequence of stream-json lines into growing text.
//
// It exists because the two line shapes overlap. Deltas arrive while the agent
// writes; the assistant line then repeats the same block in full. Appending
// both would show every sentence twice, so a completed block replaces the
// deltas that built it rather than adding to them.
type streamReader struct {
	// done is text from blocks the agent has finished.
	done strings.Builder
	// pending is the current block, assembled from deltas.
	pending strings.Builder
}

// push consumes one line and reports whether the visible text changed.
func (r *streamReader) push(line []byte) bool {
	text, final := streamText(line)
	if text == "" {
		return false
	}
	if final {
		// The finished block supersedes the deltas that built it.
		r.done.WriteString(text)
		r.pending.Reset()
		return true
	}
	r.pending.WriteString(text)
	return true
}

// text is everything the agent has written so far.
func (r *streamReader) text() string {
	return r.done.String() + r.pending.String()
}
