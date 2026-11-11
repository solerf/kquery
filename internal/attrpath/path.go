// Package attrpath parses caller-typed attribute paths and resolves them
// against unstructured Kubernetes objects (the map[string]interface{} form the
// dynamic client returns), rendering the result as a single output cell.
package attrpath

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// segment is one step of a path: a map key or a list index.
type segment struct {
	key   string
	index int
	isIdx bool
}

// Path is a parsed attribute path.
type Path struct {
	raw      string
	segments []segment
}

// verbatimPrefixes address the two metadata string maps whose keys contain
// dots and slashes (app.kubernetes.io/name). Everything after the prefix is
// the map key as typed; it is never split.
var verbatimPrefixes = []struct {
	prefix string
	base   []string
}{
	{"labels.", []string{"metadata", "labels"}},
	{"annotations.", []string{"metadata", "annotations"}},
	{"metadata.labels.", []string{"metadata", "labels"}},
	{"metadata.annotations.", []string{"metadata", "annotations"}},
}

// Parse turns a user-typed path into a Path.
//
// "labels.<key>" and "annotations.<key>" resolve to metadata.labels[<key>] and
// metadata.annotations[<key>] with <key> taken verbatim. Any other path is
// split on "." and each segment may carry list indices: "containers[0].image".
func Parse(raw string) (Path, error) {
	if strings.TrimSpace(raw) == "" {
		return Path{}, fmt.Errorf("empty attribute path")
	}
	switch raw {
	case "labels", "annotations":
		return Path{raw: raw, segments: []segment{{key: "metadata"}, {key: raw}}}, nil
	}
	for _, vp := range verbatimPrefixes {
		if !strings.HasPrefix(raw, vp.prefix) {
			continue
		}
		key := raw[len(vp.prefix):]
		if key == "" {
			return Path{}, fmt.Errorf("attribute path %q: missing key after %q", raw, vp.prefix)
		}
		segs := make([]segment, 0, len(vp.base)+1)
		for _, k := range vp.base {
			segs = append(segs, segment{key: k})
		}
		segs = append(segs, segment{key: key})
		return Path{raw: raw, segments: segs}, nil
	}

	var segs []segment
	for _, part := range strings.Split(raw, ".") {
		s, err := parseSegment(part)
		if err != nil {
			return Path{}, fmt.Errorf("attribute path %q: %w", raw, err)
		}
		segs = append(segs, s...)
	}
	return Path{raw: raw, segments: segs}, nil
}

// parseSegment parses "name", "name[0]" or "name[0][1]" into a key segment
// followed by zero or more index segments.
func parseSegment(part string) ([]segment, error) {
	if part == "" {
		return nil, fmt.Errorf("empty segment")
	}
	open := strings.IndexByte(part, '[')
	if open < 0 {
		return []segment{{key: part}}, nil
	}
	if open == 0 {
		return nil, fmt.Errorf("segment %q: index without a field name", part)
	}
	segs := []segment{{key: part[:open]}}
	rest := part[open:]
	for rest != "" {
		if rest[0] != '[' {
			return nil, fmt.Errorf("segment %q: unexpected %q after index", part, rest)
		}
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return nil, fmt.Errorf("segment %q: unterminated index", part)
		}
		n, err := strconv.Atoi(rest[1:end])
		if err != nil || n < 0 {
			return nil, fmt.Errorf("segment %q: index must be a non-negative integer", part)
		}
		segs = append(segs, segment{index: n, isIdx: true})
		rest = rest[end+1:]
	}
	return segs, nil
}

// String returns the path exactly as typed; it doubles as the column header.
func (p Path) String() string { return p.raw }

// Resolve walks obj along the path and renders the value with Format. A
// missing key, an index out of range, or a step through a non-container
// yields "".
func (p Path) Resolve(obj map[string]interface{}) string {
	var cur interface{} = obj
	for _, s := range p.segments {
		if s.isIdx {
			list, ok := cur.([]interface{})
			if !ok || s.index >= len(list) {
				return ""
			}
			cur = list[s.index]
			continue
		}
		m, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		v, found := m[s.key]
		if !found {
			return ""
		}
		cur = v
	}
	return Format(cur)
}

// Format renders a decoded JSON value as one cell: scalars in their string
// form, maps and lists as compact JSON, nil as "".
func Format(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case map[string]interface{}, []interface{}:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	default:
		return fmt.Sprint(t)
	}
}
