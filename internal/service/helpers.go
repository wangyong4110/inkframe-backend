package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

// extractJSON extracts the first JSON object or array from text that may contain
// markdown code fences or other surrounding content.
// This is the single canonical implementation for the service package — do NOT
// duplicate this function in other service files.
//
// 从 AI 输出中提取纯 JSON 字符串。
// 优先提取数组（[...]）；若顶层是对象（{...}），尝试查找其内部的第一个数组。
func extractJSON(content string) string {
	content = strings.TrimSpace(content)
	if idx := strings.Index(content, "```json"); idx != -1 {
		content = content[idx+7:]
		if end := strings.Index(content, "```"); end != -1 {
			content = content[:end]
		}
	} else if idx := strings.Index(content, "```"); idx != -1 {
		content = content[idx+3:]
		if end := strings.Index(content, "```"); end != -1 {
			content = content[:end]
		}
	}
	content = strings.TrimSpace(content)

	// extractBracket extracts a balanced bracket expression starting at `start`.
	extractBracket := func(s string, start int, open, close byte) string {
		depth := 0
		inStr := false
		for i := start; i < len(s); i++ {
			c := s[i]
			if inStr {
				if c == '\\' {
					i++
				} else if c == '"' {
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					return sanitizeJSONStrings(s[start : i+1])
				}
			}
		}
		return sanitizeJSONStrings(s[start:])
	}

	// Prefer array over object: find the first '[' and the first '{'.
	arrIdx := strings.Index(content, "[")
	objIdx := strings.Index(content, "{")
	if arrIdx != -1 && (objIdx == -1 || arrIdx <= objIdx) {
		// Array comes first — extract it directly.
		return extractBracket(content, arrIdx, '[', ']')
	}
	if objIdx != -1 {
		// Top-level is an object. Extract it, then look for an embedded array.
		obj := extractBracket(content, objIdx, '{', '}')
		// Search for the first '[' inside the object to unwrap {"shots":[...]} style.
		if inner := strings.Index(obj, "["); inner != -1 {
			return extractBracket(obj, inner, '[', ']')
		}
		return obj
	}
	return content
}

// repairTruncatedJSONArray 尝试修复被截断的 JSON 数组。
// 找到最后一个完整的对象元素（depth 从 1 降回 1 的 '}'），在该处截断并补 ']'。
// 返回修复后的字符串；若无法修复则返回原始字符串。
func repairTruncatedJSONArray(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return s
	}
	depth := 0
	inStr := false
	lastCompleteEnd := -1 // position (exclusive) after the last '}' at array depth 1→0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\\' {
				i++ // skip escaped char
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[', '{':
			depth++
		case '}':
			depth--
			if depth == 1 { // just closed an array element (not nested object)
				lastCompleteEnd = i + 1
			}
		case ']':
			depth--
		}
	}
	if lastCompleteEnd > 0 && depth > 0 {
		return s[:lastCompleteEnd] + "]"
	}
	return s
}

// sanitizeJSONStrings 将 JSON 字符串字面量内未转义的控制字符（\n \r \t）
// 替换为合法的转义序列，修复 LLM 有时直接输出裸换行的问题。
func sanitizeJSONStrings(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch c {
			case '\\': // 已有转义序列，原样保留两个字节
				buf.WriteByte(c)
				i++
				if i < len(s) {
					buf.WriteByte(s[i])
				}
			case '"':
				inStr = false
				buf.WriteByte(c)
			case '\n':
				buf.WriteString(`\n`)
			case '\r':
				buf.WriteString(`\r`)
			case '\t':
				buf.WriteString(`\t`)
			default:
				buf.WriteByte(c)
			}
		} else {
			if c == '"' {
				inStr = true
			}
			buf.WriteByte(c)
		}
	}
	return buf.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// countChineseChars counts the number of CJK Unified Ideograph runes in text.
func countChineseChars(text string) int {
	count := 0
	for _, r := range text {
		if r >= 0x4e00 && r <= 0x9fa5 {
			count++
		}
	}
	return count
}

// sanitizeStorageName strips unsafe characters and limits length to 64 runes.
func sanitizeStorageName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified Ideographs
			b.WriteRune(r)
		}
	}
	runes := []rune(b.String())
	if len(runes) > 64 {
		runes = runes[:64]
	}
	return string(runes)
}

// extractJSONObject extracts the first top-level JSON object {...} from an AI response,
// without unwrapping any inner arrays. Use this when the expected result is an object,
// not an array (contrast with extractJSON which prefers arrays for storyboard use).
func extractJSONObject(content string) string {
	content = strings.TrimSpace(content)
	if idx := strings.Index(content, "```json"); idx != -1 {
		content = content[idx+7:]
		if end := strings.Index(content, "```"); end != -1 {
			content = content[:end]
		}
	} else if idx := strings.Index(content, "```"); idx != -1 {
		content = content[idx+3:]
		if end := strings.Index(content, "```"); end != -1 {
			content = content[:end]
		}
	}
	content = strings.TrimSpace(content)
	objIdx := strings.Index(content, "{")
	if objIdx == -1 {
		return content
	}
	depth := 0
	inStr := false
	for i := objIdx; i < len(content); i++ {
		c := content[i]
		if inStr {
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return sanitizeJSONStrings(content[objIdx : i+1])
			}
		}
	}
	return sanitizeJSONStrings(content[objIdx:])
}

// unmarshalAIJSON extracts JSON from an AI response string and unmarshals it into T.
func unmarshalAIJSON[T any](raw string) (*T, error) {
	cleaned := extractJSON(strings.TrimSpace(raw))
	var result T
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return nil, fmt.Errorf("unmarshalAIJSON: %w (raw: %s)", err, cleaned[:min(200, len(cleaned))])
	}
	return &result, nil
}

