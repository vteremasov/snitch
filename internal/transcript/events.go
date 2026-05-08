package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Kind int

const (
	KindOther Kind = iota
	KindUserMessage
	KindAssistantText
	KindToolUse
	KindToolResult
)

type Event struct {
	Kind         Kind
	Tool         string
	ToolUseID    string
	InputPreview string
	Text         string
	Timestamp    time.Time
}

func (k Kind) String() string {
	switch k {
	case KindUserMessage:
		return "user"
	case KindAssistantText:
		return "assistant"
	case KindToolUse:
		return "tool_use"
	case KindToolResult:
		return "tool_result"
	default:
		return "other"
	}
}

// Summary returns a short single-line description suitable for the dashboard.
func (e Event) Summary() string {
	switch e.Kind {
	case KindToolUse:
		if e.InputPreview != "" {
			return fmt.Sprintf("%s: %s", e.Tool, e.InputPreview)
		}
		return e.Tool
	case KindToolResult:
		return fmt.Sprintf("← %s", e.Tool)
	case KindUserMessage:
		return truncate(e.Text, 80)
	case KindAssistantText:
		return truncate(e.Text, 80)
	}
	return ""
}

// Classify decodes a single jsonl line (already parsed into a map). Claude
// Code's transcript schema isn't a stable contract, so we look up fields
// permissively and fall back to KindOther when the shape is unfamiliar.
func Classify(m map[string]any) Event {
	e := Event{Kind: KindOther}

	if ts, ok := m["timestamp"].(string); ok {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			e.Timestamp = t
		}
	}

	msg, _ := m["message"].(map[string]any)
	if msg == nil {
		return e
	}
	role, _ := msg["role"].(string)
	content := msg["content"]

	switch role {
	case "user":
		// Tool results arrive as user messages with content blocks of type "tool_result".
		if items, ok := content.([]any); ok {
			for _, raw := range items {
				it, _ := raw.(map[string]any)
				if it == nil {
					continue
				}
				if t, _ := it["type"].(string); t == "tool_result" {
					e.Kind = KindToolResult
					e.ToolUseID, _ = it["tool_use_id"].(string)
					return e
				}
			}
			// fall through to a plain user message — text payload
		}
		e.Kind = KindUserMessage
		e.Text = stringContent(content)

	case "assistant":
		if items, ok := content.([]any); ok {
			for _, raw := range items {
				it, _ := raw.(map[string]any)
				if it == nil {
					continue
				}
				switch t, _ := it["type"].(string); t {
				case "tool_use":
					e.Kind = KindToolUse
					e.Tool, _ = it["name"].(string)
					e.ToolUseID, _ = it["id"].(string)
					if input, ok := it["input"].(map[string]any); ok {
						e.InputPreview = previewInput(e.Tool, input)
					}
					return e
				case "text":
					if e.Kind == KindOther {
						e.Kind = KindAssistantText
						s, _ := it["text"].(string)
						e.Text = s
					}
				}
			}
		} else {
			e.Kind = KindAssistantText
			e.Text = stringContent(content)
		}
	}
	return e
}

func stringContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			it, _ := raw.(map[string]any)
			if it == nil {
				continue
			}
			if s, ok := it["text"].(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// previewInput renders a short preview tailored to common tools. Falls back
// to a short JSON snippet for anything else.
func previewInput(tool string, input map[string]any) string {
	switch tool {
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			return truncate(cmd, 80)
		}
	case "Read", "Edit", "Write":
		if p, ok := input["file_path"].(string); ok {
			return truncate(p, 80)
		}
	case "Grep":
		if p, ok := input["pattern"].(string); ok {
			return truncate(p, 80)
		}
	}
	b, _ := json.Marshal(input)
	return truncate(string(b), 80)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
