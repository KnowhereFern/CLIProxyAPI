package conversation

import (
	"regexp"
	"strings"
)

// NormalizeRole converts a role to a standard format (lowercase, 'model' -> 'assistant').
func NormalizeRole(role string) string {
	r := strings.ToLower(role)
	if r == "model" {
		return "assistant"
	}
	return r
}

// NeedRoleTags checks if a list of messages requires role tags.
func NeedRoleTags(msgs []Message) bool {
	for _, m := range msgs {
		if strings.ToLower(m.Role) != "user" {
			return true
		}
	}
	return false
}

// AddRoleTag wraps content with a role tag.
func AddRoleTag(role, content string, unclose bool) string {
	if role == "" {
		role = "user"
	}
	if unclose {
		return "<|im_start|>" + role + "\n" + content
	}
	return "<|im_start|>" + role + "\n" + content + "\n<|im_end|>"
}

// BuildPrompt constructs the final prompt from a list of messages.
func BuildPrompt(msgs []Message, tagged bool, appendAssistant bool) string {
	if len(msgs) == 0 {
		if tagged && appendAssistant {
			return AddRoleTag("assistant", "", true)
		}
		return ""
	}
	if !tagged {
		var sb strings.Builder
		for i, m := range msgs {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(m.Text)
		}
		return sb.String()
	}
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(AddRoleTag(m.Role, m.Text, false))
		sb.WriteString("\n")
	}
	if appendAssistant {
		sb.WriteString(AddRoleTag("assistant", "", true))
	}
	return strings.TrimSpace(sb.String())
}

var reXMLAnyTag = regexp.MustCompile(`(?s)<\s*[^>]+>`)

// AppendXMLWrapHintIfNeeded appends an XML wrap hint to messages containing XML-like blocks.
func AppendXMLWrapHintIfNeeded(msgs []Message, disable bool) []Message {
	if disable {
		return msgs
	}
	const xmlWrapHint = "\nFor any xml block, e.g. tool call, always wrap it with: \n`````xml\n...\n`````\n"
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		t := m.Text
		if reXMLAnyTag.MatchString(t) {
			t = t + xmlWrapHint
		}
		out = append(out, Message{Role: m.Role, Text: t})
	}
	return out
}
