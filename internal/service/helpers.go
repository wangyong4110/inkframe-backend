package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// buildCrawlHTTPClient returns an HTTP client for crawling external sites.
// proxyURL may be empty (falls back to HTTPS_PROXY / HTTP_PROXY env vars via
// http.ProxyFromEnvironment), or a full URL like "http://127.0.0.1:7890" or
// "socks5://127.0.0.1:1080".
func buildCrawlHTTPClient(proxyURL string, timeout time.Duration) *http.Client {
	proxyFn := http.ProxyFromEnvironment
	if proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			proxyFn = http.ProxyURL(parsed)
		}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               proxyFn,
			TLSHandshakeTimeout: 10 * time.Second,
			DisableKeepAlives:   true,
		},
	}
}

// stripMarkdownFences 剥除 AI 输出中的 ```json 或 ``` 代码围栏，返回纯内容。
func stripMarkdownFences(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "```json"); idx != -1 {
		s = s[idx+7:]
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	} else if idx := strings.Index(s, "```"); idx != -1 {
		s = s[idx+3:]
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	}
	return strings.TrimSpace(s)
}

// extractJSON extracts the first JSON object or array from text that may contain
// markdown code fences or other surrounding content.
// This is the single canonical implementation for the service package — do NOT
// duplicate this function in other service files.
//
// 从 AI 输出中提取纯 JSON 字符串。
// 优先提取数组（[...]）；若顶层是对象（{...}），尝试查找其内部的第一个数组。
func extractJSON(content string) string {
	content = stripMarkdownFences(content)

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

// extractJSONAuto picks the right extractor based on whether the AI response is an object or
// array. For objects, it preserves the full structure (no inner-array unwrapping). For arrays
// it falls through to extractJSON which handles truncated arrays and bare arrays.
// This is the correct extractor for generateJSONForTenantCtx: storyboard tasks that return
// {"shots":[...]} used extractJSON which silently discarded the object wrapper; tasks that
// return multi-key objects like {"new_anchors":[...],"appearing_anchors":[...]} need the
// full object to survive.
func extractJSONAuto(content string) string {
	stripped := stripMarkdownFences(content)
	objIdx := strings.Index(stripped, "{")
	arrIdx := strings.Index(stripped, "[")
	if objIdx != -1 && (arrIdx == -1 || objIdx < arrIdx) {
		return extractJSONObject(content)
	}
	return extractJSON(content)
}

// extractJSONObject extracts the first top-level JSON object {...} from an AI response,
// without unwrapping any inner arrays. Use this when the expected result is an object,
// not an array (contrast with extractJSON which prefers arrays for storyboard use).
func extractJSONObject(content string) string {
	content = stripMarkdownFences(content)
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

// repairMissingColons fixes a common DeepSeek JSON issue where object key-value
// pairs are missing the colon separator, e.g. {"key" true} → {"key": true}.
//
// Strategy: scan the JSON token by token. After every quoted string token, peek
// at the next non-whitespace character. If it is not `:`, `,`, `}`, or `]`
// (i.e., something that would be a bare JSON value), insert ": " before it.
// This is safe to call on well-formed JSON — it only inserts colons where they
// are truly absent.
func repairMissingColons(s string) string {
	n := len(s)
	if n == 0 {
		return s
	}
	var buf strings.Builder
	buf.Grow(n + 16)
	i := 0
	for i < n {
		c := s[i]
		if c != '"' {
			buf.WriteByte(c)
			i++
			continue
		}
		// Consume a quoted string (handles \\ escape sequences).
		start := i
		i++
		for i < n {
			if s[i] == '\\' {
				i += 2 // skip escape and next byte
			} else if s[i] == '"' {
				i++
				break
			} else {
				i++
			}
		}
		buf.WriteString(s[start:i])

		// Peek past whitespace to see what follows this string.
		j := i
		for j < n && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
			j++
		}
		if j >= n {
			continue
		}
		next := s[j]
		if next == ':' || next == ',' || next == '}' || next == ']' {
			// Well-formed — no insertion needed.
			continue
		}
		// next is a value token (true/false/null/digit/[/{/") without a colon.
		// Write the whitespace we skipped, then insert the missing colon.
		buf.WriteString(s[i:j])
		buf.WriteString(": ")
		i = j
	}
	return buf.String()
}

// corruptedTitleKeyRe matches the pattern where an AI output omits the field name
// and colon for a chapter title, producing:  ,"" ChineseTitle"
// instead of:                                ,"title": "ChineseTitle"
//
// Root cause: the model emits `"title": "ChineseTitle"` but the leading `title": `
// (8 bytes) gets truncated or corrupted, leaving an empty key followed by the
// value text without its opening quote.  The pattern is:
//
//	comma + optional whitespace + "" + non-quote text + closing quote
var corruptedTitleKeyRe = regexp.MustCompile(`,(\s*)""([^"]+)"`)

// repairCorruptedTitleKey fixes the pattern ,"" ChineseTitle" → ,"title": "ChineseTitle".
// It is safe to call on well-formed JSON because a bare empty-string key followed
// directly by unquoted text is always invalid JSON.
func repairCorruptedTitleKey(s string) string {
	return corruptedTitleKeyRe.ReplaceAllString(s, `,$1"title": "$2"`)
}

// repairAIJSON 修复 AI 生成的 JSON 中常见的两类问题：
//  1. 字段间出现中文注释/说明文字（非 ASCII 字节出现在字符串字面量外）
//  2. 因上述中文文字替换了逗号分隔符后导致的缺失逗号
//
// 修复流程：
//   step-1 stripNonAsciiOutsideStrings — 移除 JSON 字符串外的非 ASCII 字节序列（UTF-8 多字节）
//   step-2 insertMissingCommasJSON     — 在相邻值之间插入缺失的逗号
func repairAIJSON(s string) string {
	s = stripNonAsciiOutsideStrings(s)
	s = insertMissingCommasJSON(s)
	return s
}

// stripNonAsciiOutsideStrings 移除 JSON 字符串字面量外的非 ASCII 字节序列（包括中文字符）。
// JSON 合法结构字符（{} [] : ,）均在 ASCII 范围内，因此此操作不影响 JSON 结构。
func stripNonAsciiOutsideStrings(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	inStr := false
	i := 0
	for i < len(s) {
		b := s[i]
		if inStr {
			switch {
			case b == '\\':
				buf.WriteByte(b)
				i++
				if i < len(s) {
					buf.WriteByte(s[i])
					i++
				}
			case b == '"':
				inStr = false
				buf.WriteByte(b)
				i++
			default:
				buf.WriteByte(b)
				i++
			}
		} else {
			if b == '"' {
				inStr = true
				buf.WriteByte(b)
				i++
			} else if b >= 0x80 {
				// 跳过完整的 UTF-8 多字节序列（0x80-0xBF 为续字节）
				i++
				for i < len(s) && (s[i]&0xC0) == 0x80 {
					i++
				}
			} else {
				buf.WriteByte(b)
				i++
			}
		}
	}
	return buf.String()
}

// insertMissingCommasJSON 在 JSON 中相邻值之间插入缺失的逗号。
// 当 AI 输出的中文注释被移除后，原先夹在值之间的逗号可能随之消失，此函数负责补全。
// 规则：在完整的 JSON 值（], }, 字符串, 字面量）之后，如果下一个非空白 token 是
// 新的值起始符（", [, {），则插入 ","。
func insertMissingCommasJSON(s string) string {
	n := len(s)
	var buf strings.Builder
	buf.Grow(n + 64)
	i := 0
	prevWasValue := false // 上一个有意义 token 是否是完整值的结尾

	for i < n {
		c := s[i]

		// 空白原样写入
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			buf.WriteByte(c)
			i++
			continue
		}

		// 分隔符复位标志
		if c == ',' || c == ':' {
			prevWasValue = false
			buf.WriteByte(c)
			i++
			continue
		}

		// 容器起始 — 不是"值"，不触发逗号插入
		if c == '[' || c == '{' {
			prevWasValue = false
			buf.WriteByte(c)
			i++
			continue
		}

		// 容器结束 — 是"值"
		if c == ']' || c == '}' {
			buf.WriteByte(c)
			i++
			prevWasValue = true
			continue
		}

		// 字符串
		if c == '"' {
			if prevWasValue {
				buf.WriteByte(',') // 补逗号
			}
			prevWasValue = false
			start := i
			i++ // 跳过开头 "
			for i < n {
				if s[i] == '\\' {
					i += 2
				} else if s[i] == '"' {
					i++
					break
				} else {
					i++
				}
			}
			buf.WriteString(s[start:i])
			prevWasValue = true
			continue
		}

		// 数字 / 字面量（true false null）
		if c == '-' || (c >= '0' && c <= '9') || c == 't' || c == 'f' || c == 'n' {
			if prevWasValue {
				buf.WriteByte(',')
			}
			prevWasValue = false
			start := i
			for i < n {
				ch := s[i]
				if ch == ',' || ch == '}' || ch == ']' || ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
					break
				}
				i++
			}
			buf.WriteString(s[start:i])
			prevWasValue = true
			continue
		}

		// 其他字符（不应出现，直接写入保留原样）
		buf.WriteByte(c)
		i++
	}
	return buf.String()
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

