package util

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
)

// deniedSVGElements are tags capable of executing script or loading arbitrary
// external/embedded content. They are dropped (with their entire subtree)
// rather than just unwrapped, since their content is meaningless without them.
var deniedSVGElements = map[string]bool{
	"script":           true,
	"foreignobject":    true,
	"iframe":           true,
	"embed":            true,
	"object":           true,
	"animate":          true,
	"animatetransform": true,
	"animatemotion":    true,
	"set":              true,
	// <style> is dropped entirely rather than sanitized: CSS supports
	// url(javascript:...), @import of external stylesheets, and other
	// injection vectors that are hard to enumerate safely.
	"style": true,
}

// allowedURLSchemePrefixes are the only href/src/xlink:href values permitted
// through the sanitizer, checked after stripping whitespace/control chars and
// lowercasing (see filterSVGAttrs). Everything else — javascript:, vbscript:,
// data: (including data:image/svg+xml, which can smuggle a nested script via
// a second sanitizer bypass), obfuscated schemes, etc. — is dropped.
var allowedURLSchemePrefixes = []string{"#", "/", "http:", "https:", "mailto:"}

// SanitizeSVG strips script-capable constructs from an SVG document so it is
// safe to store and later serve/render in a browser. It rejects the input if
// it does not parse as well-formed XML with an <svg> root element.
//
// Go's encoding/xml never resolves DTD entities (no XXE risk), but DOCTYPE/
// ENTITY declarations and non-xml processing instructions (e.g.
// xml-stylesheet, which some browsers use to load and execute external XSLT)
// are dropped anyway as defense in depth.
func SanitizeSVG(data []byte) ([]byte, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))

	var out bytes.Buffer
	enc := xml.NewEncoder(&out)

	sawRoot := false
	// depth counts nesting inside a denied element's subtree; while >0 all
	// tokens (including nested start/end elements and text) are discarded.
	depth := 0

	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("invalid svg: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if depth > 0 {
				if deniedSVGElements[name] {
					depth++
				}
				continue
			}
			if deniedSVGElements[name] {
				depth = 1
				continue
			}
			if !sawRoot {
				if name != "svg" {
					return nil, fmt.Errorf("invalid svg: root element is %q, not <svg>", t.Name.Local)
				}
				sawRoot = true
			}
			t.Attr = filterSVGAttrs(t.Attr)
			if err := enc.EncodeToken(t); err != nil {
				return nil, err
			}
		case xml.EndElement:
			if depth > 0 {
				depth--
				continue
			}
			if err := enc.EncodeToken(t); err != nil {
				return nil, err
			}
		case xml.Directive:
			// Drop <!DOCTYPE ...>, <!ENTITY ...>, etc.
			continue
		case xml.ProcInst:
			if t.Target != "xml" {
				// Drop xml-stylesheet and any other processing instruction.
				continue
			}
			if err := enc.EncodeToken(t); err != nil {
				return nil, err
			}
		case xml.Comment:
			continue
		default:
			if depth > 0 {
				continue
			}
			if err := enc.EncodeToken(t); err != nil {
				return nil, err
			}
		}
	}

	if !sawRoot {
		return nil, fmt.Errorf("invalid svg: no <svg> root element found")
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func filterSVGAttrs(attrs []xml.Attr) []xml.Attr {
	filtered := make([]xml.Attr, 0, len(attrs))
	for _, a := range attrs {
		local := strings.ToLower(a.Name.Local)
		if strings.HasPrefix(local, "on") {
			continue // event handlers: onload, onclick, onerror, ...
		}
		if local == "href" || local == "src" {
			if !hasAllowedURLScheme(a.Value) {
				continue
			}
		}
		filtered = append(filtered, a)
	}
	return filtered
}

// hasAllowedURLScheme reports whether v — an href/src attribute value —
// starts with one of allowedURLSchemePrefixes once whitespace and C0/DEL
// control characters (which browsers ignore when parsing a URL scheme,
// enabling denylist bypasses like "java\tscript:") are stripped and the
// result is lowercased.
func hasAllowedURLScheme(v string) bool {
	v = strings.Map(func(r rune) rune {
		if r <= 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
	v = strings.ToLower(v)
	for _, prefix := range allowedURLSchemePrefixes {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}
