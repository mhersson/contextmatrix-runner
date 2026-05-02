// Package claudeclient wraps Claude Code subprocess invocation: spawning
// processes via Docker SDK, parsing stream-json output, recognizing
// structured terminal markers, and capturing token usage.
package claudeclient

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// EventKind classifies one event in CC's stream-json output.
type EventKind string

const (
	EventSystemInit EventKind = "system_init"
	EventSystemEnd  EventKind = "system_end"
	EventText       EventKind = "text"
	EventThinking   EventKind = "thinking"
	EventToolUse    EventKind = "tool_use"
	EventToolResult EventKind = "tool_result"
	EventError      EventKind = "error"
)

// Usage captures token counters reported by CC at end-of-turn.
type Usage struct {
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Model        string `json:"model,omitempty"`
}

// StreamEvent is one classified event read from a CC stream-json source.
type StreamEvent struct {
	Kind      EventKind
	Text      string                 // for text/thinking
	SessionID string                 // for system_init
	Model     string                 // for system_init
	ToolName  string                 // for tool_use
	ToolInput json.RawMessage        // for tool_use
	ToolUseID string                 // for tool_result
	Content   string                 // for tool_result (rendered)
	Usage     Usage                  // for system_end
	Raw       map[string]interface{} // full original event for callers needing more
}

// StreamJSONParser reads JSON-per-line from src, classifying each line
// into one or more typed StreamEvents. Claude Code's stream-json emits
// envelope frames (`assistant`, `user`, `system`, `result`, `rate_limit_event`),
// some of which carry multiple inner content blocks. This parser flattens
// each line into the per-block events the orchestrator's phase actions
// expect (text/thinking/tool_use/tool_result/system_init/system_end).
type StreamJSONParser struct {
	src       io.ReadCloser
	events    chan StreamEvent
	err       error
	lastModel string // captured from system_init, applied to system_end Usage.Model
}

// NewStreamJSONParser starts a goroutine that consumes src.
func NewStreamJSONParser(src io.ReadCloser) *StreamJSONParser {
	p := &StreamJSONParser{
		src:    src,
		events: make(chan StreamEvent, 64),
	}
	go p.run()

	return p
}

// Events returns the channel that receives parsed events. Closes when src is exhausted.
func (p *StreamJSONParser) Events() <-chan StreamEvent { return p.events }

// Err returns the first parse/read error encountered, or nil.
func (p *StreamJSONParser) Err() error { return p.err }

// Close stops the parser by closing the source ReadCloser.
func (p *StreamJSONParser) Close() error {
	return p.src.Close()
}

func (p *StreamJSONParser) run() {
	defer close(p.events)
	defer p.src.Close()

	sc := bufio.NewScanner(p.src)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(line, &raw); err != nil {
			p.err = fmt.Errorf("parse stream-json: %w (line: %s)", err, line)

			return
		}

		for _, ev := range p.classify(raw) {
			p.events <- ev
		}
	}

	if err := sc.Err(); err != nil {
		p.err = fmt.Errorf("read stream-json: %w", err)
	}
}

// classify expands one stream-json envelope into zero or more typed events.
// Claude Code's actual format (verified against claude-code 2.1.x):
//
//   - {"type":"system","subtype":"init", session_id, model, ...}        → system_init
//   - {"type":"assistant","message":{"content":[<blocks>], "usage":{...}}} → one event per block + system_end
//   - {"type":"user","message":{"content":[{"type":"tool_result", ...}]}}  → tool_result events
//   - {"type":"result", usage, ...}                                      → system_end
//   - {"type":"rate_limit_event", ...}                                   → ignored
//   - {"type":"error", ...}                                              → error
//
// The legacy flat shape ({"type":"text","text":"..."}) is also still
// recognized so a future format flip back doesn't silently regress.
func (p *StreamJSONParser) classify(raw map[string]interface{}) []StreamEvent {
	typ, _ := raw["type"].(string)

	switch typ {
	case "system":
		return p.classifySystem(raw)
	case "assistant":
		return p.classifyAssistant(raw)
	case "user":
		return p.classifyUser(raw)
	case "result":
		return []StreamEvent{p.endEventFromResult(raw)}
	case "text":
		text, _ := raw["text"].(string)

		return []StreamEvent{{Kind: EventText, Text: text, Raw: raw}}
	case "thinking":
		text, _ := raw["text"].(string)

		return []StreamEvent{{Kind: EventThinking, Text: text, Raw: raw}}
	case "tool_use":
		return []StreamEvent{p.toolUseFromMap(raw, raw)}
	case "tool_result":
		ev := StreamEvent{Kind: EventToolResult, Raw: raw}
		ev.ToolUseID, _ = raw["tool_use_id"].(string)

		if c, ok := raw["content"].(string); ok {
			ev.Content = c
		}

		return []StreamEvent{ev}
	case "error":
		text, _ := raw["message"].(string)

		return []StreamEvent{{Kind: EventError, Text: text, Raw: raw}}
	case "rate_limit_event":
		return nil
	}

	return nil
}

func (p *StreamJSONParser) classifySystem(raw map[string]interface{}) []StreamEvent {
	sub, _ := raw["subtype"].(string)

	switch sub {
	case "init":
		ev := StreamEvent{Kind: EventSystemInit, Raw: raw}
		ev.SessionID, _ = raw["session_id"].(string)
		ev.Model, _ = raw["model"].(string)

		if ev.Model != "" {
			p.lastModel = ev.Model
		}

		return []StreamEvent{ev}
	case "end":
		if u, ok := raw["usage"].(map[string]interface{}); ok {
			return []StreamEvent{p.endEventFromUsageMap(raw, u)}
		}

		return []StreamEvent{{Kind: EventSystemEnd, Raw: raw, Model: p.lastModel}}
	}

	return nil
}

func (p *StreamJSONParser) classifyAssistant(raw map[string]interface{}) []StreamEvent {
	msg, _ := raw["message"].(map[string]interface{})
	if msg == nil {
		return nil
	}

	var out []StreamEvent

	if model, ok := msg["model"].(string); ok && model != "" {
		p.lastModel = model
	}

	content, _ := msg["content"].([]interface{})
	for _, raw := range content {
		block, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		btype, _ := block["type"].(string)
		switch btype {
		case "text":
			text, _ := block["text"].(string)
			out = append(out, StreamEvent{Kind: EventText, Text: text, Raw: block})
		case "thinking":
			// Claude emits {"type":"thinking","thinking":"..."}; legacy used "text".
			text, _ := block["thinking"].(string)
			if text == "" {
				text, _ = block["text"].(string)
			}

			out = append(out, StreamEvent{Kind: EventThinking, Text: text, Raw: block})
		case "tool_use":
			out = append(out, p.toolUseFromMap(block, block))
		}
	}

	// If the assistant message carries a usage block, surface it as a
	// system_end so phase actions that rely on token counters see them
	// even when Claude doesn't emit a separate `result` frame.
	if usageMap, ok := msg["usage"].(map[string]interface{}); ok {
		out = append(out, p.endEventFromUsageMap(raw, usageMap))
	}

	return out
}

func (p *StreamJSONParser) classifyUser(raw map[string]interface{}) []StreamEvent {
	msg, _ := raw["message"].(map[string]interface{})
	if msg == nil {
		return nil
	}

	content, _ := msg["content"].([]interface{})

	var out []StreamEvent

	for _, raw := range content {
		block, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		btype, _ := block["type"].(string)
		if btype != "tool_result" {
			continue
		}

		ev := StreamEvent{Kind: EventToolResult, Raw: block}
		ev.ToolUseID, _ = block["tool_use_id"].(string)

		// content may be a string or an array of {type:text,text:...} blocks.
		switch c := block["content"].(type) {
		case string:
			ev.Content = c
		case []interface{}:
			var sb string

			for _, sub := range c {
				if m, ok := sub.(map[string]interface{}); ok {
					if t, _ := m["text"].(string); t != "" {
						sb += t
					}
				}
			}

			ev.Content = sb
		}

		out = append(out, ev)
	}

	return out
}

// toolUseFromMap pulls name/input from either the legacy flat shape
// (raw is the tool_use envelope) or the new nested shape (raw is a
// content block). Both layouts use the same field names.
func (p *StreamJSONParser) toolUseFromMap(_, src map[string]interface{}) StreamEvent {
	ev := StreamEvent{Kind: EventToolUse, Raw: src}
	ev.ToolName, _ = src["name"].(string)

	if input, ok := src["input"].(map[string]interface{}); ok {
		b, _ := json.Marshal(input)
		ev.ToolInput = b
	}

	return ev
}

// endEventFromResult builds an EventSystemEnd from a top-level
// {"type":"result", usage:{...}, total_cost_usd:..., ...} frame.
func (p *StreamJSONParser) endEventFromResult(raw map[string]interface{}) StreamEvent {
	if u, ok := raw["usage"].(map[string]interface{}); ok {
		return p.endEventFromUsageMap(raw, u)
	}

	return StreamEvent{Kind: EventSystemEnd, Raw: raw, Model: p.lastModel}
}

// endEventFromUsageMap builds an EventSystemEnd given a usage map.
func (p *StreamJSONParser) endEventFromUsageMap(raw, u map[string]interface{}) StreamEvent {
	ev := StreamEvent{Kind: EventSystemEnd, Raw: raw}

	if m, ok := raw["model"].(string); ok && m != "" {
		ev.Model = m
	} else {
		ev.Model = p.lastModel
	}

	ev.Usage = Usage{
		InputTokens:  toInt(u["input_tokens"]),
		OutputTokens: toInt(u["output_tokens"]),
		Model:        ev.Model,
	}

	return ev
}

func toInt(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}

	return 0
}
