package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// ruleSet is the wire format of the rules. The flag's variation value and the
// plugin config's "rules" key share it; ../../README.md documents it.
type ruleSet struct {
	Headers struct {
		Remove []string     `json:"remove"`
		Rename []renameRule `json:"rename"`
	} `json:"headers"`
	Body struct {
		Remove []string     `json:"remove"`
		Rename []renameRule `json:"rename"`
	} `json:"body"`
}

type renameRule struct {
	Pattern     string   `json:"pattern"`
	Replacement string   `json:"replacement"`
	Target      string   `json:"target"`
	Paths       []string `json:"paths"`
}

// rules is a compiled, immutable ruleSet, safe for concurrent use.
type rules struct {
	headerRemove []*regexp.Regexp
	headerRename []headerRename
	bodyRemove   []jsonPath
	bodyRename   []bodyRename
	// prefilter holds what a body must contain for any body rule to apply, so
	// most bodies are never parsed. nil means every body has to be parsed.
	prefilter [][]byte
	summary   string
}

type headerRename struct {
	name        *regexp.Regexp // matches lower-cased header names; nil when names are not renamed
	value       *regexp.Regexp // nil when values are not rewritten
	replacement string
}

type bodyRename struct {
	re          *regexp.Regexp
	replacement string
	values      bool       // rewrite string values instead of keys
	paths       []jsonPath // where it applies; nil means the whole body
}

// jsonPath is a dot-separated path into a JSON document. "*" matches every key
// of an object, and arrays are walked transparently: a path continues into
// every element.
type jsonPath []string

func (p jsonPath) String() string { return strings.Join(p, ".") }

func parsePath(s string) (jsonPath, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty path")
	}
	segments := strings.Split(s, ".")
	for _, seg := range segments {
		if seg == "" {
			return nil, fmt.Errorf("path %q has an empty segment", s)
		}
	}
	return segments, nil
}

// decodeRules parses and compiles the wire format, refusing unknown keys so a
// typo cannot silently disable a rule.
func decodeRules(raw []byte) (*rules, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec ruleSet
	if err := dec.Decode(&spec); err != nil {
		return nil, err
	}
	return compileRules(spec)
}

func compileRules(spec ruleSet) (*rules, error) {
	r := &rules{}
	alwaysParse := false

	for _, p := range spec.Headers.Remove {
		re, err := regexp.Compile("(?i)^(?:" + p + ")$")
		if err != nil {
			return nil, fmt.Errorf("headers.remove %q: %w", p, err)
		}
		r.headerRemove = append(r.headerRemove, re)
	}
	for i, rule := range spec.Headers.Rename {
		if rule.Pattern == "" {
			return nil, fmt.Errorf("headers.rename[%d]: pattern is required", i)
		}
		if len(rule.Paths) > 0 {
			return nil, fmt.Errorf("headers.rename[%d]: paths only apply to body rules", i)
		}
		hr := headerRename{replacement: rule.Replacement}
		target := rule.Target
		if target == "" {
			target = "names"
		}
		if target != "names" && target != "values" && target != "both" {
			return nil, fmt.Errorf("headers.rename[%d]: target must be names, values or both, got %q", i, rule.Target)
		}
		if target != "values" {
			re, err := regexp.Compile("(?i)" + rule.Pattern) // header names are case-insensitive
			if err != nil {
				return nil, fmt.Errorf("headers.rename[%d] %q: %w", i, rule.Pattern, err)
			}
			hr.name = re
		}
		if target != "names" {
			re, err := regexp.Compile(rule.Pattern)
			if err != nil {
				return nil, fmt.Errorf("headers.rename[%d] %q: %w", i, rule.Pattern, err)
			}
			hr.value = re
		}
		r.headerRename = append(r.headerRename, hr)
	}

	for _, p := range spec.Body.Remove {
		path, err := parsePath(p)
		if err != nil {
			return nil, fmt.Errorf("body.remove: %w", err)
		}
		r.bodyRemove = append(r.bodyRemove, path)
		if last := path[len(path)-1]; last == "*" {
			alwaysParse = true
		} else {
			key, _ := json.Marshal(last)
			r.prefilter = append(r.prefilter, key)
		}
	}
	for i, rule := range spec.Body.Rename {
		if rule.Pattern == "" {
			return nil, fmt.Errorf("body.rename[%d]: pattern is required", i)
		}
		re, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return nil, fmt.Errorf("body.rename[%d] %q: %w", i, rule.Pattern, err)
		}
		br := bodyRename{re: re, replacement: rule.Replacement}
		switch rule.Target {
		case "", "keys":
		case "values":
			// Rewriting every string would rewrite model output too.
			if len(rule.Paths) == 0 {
				return nil, fmt.Errorf("body.rename[%d]: target values needs paths, so model output is never rewritten", i)
			}
			br.values = true
		default:
			return nil, fmt.Errorf("body.rename[%d]: target must be keys or values, got %q", i, rule.Target)
		}
		for _, p := range rule.Paths {
			path, err := parsePath(p)
			if err != nil {
				return nil, fmt.Errorf("body.rename[%d]: %w", i, err)
			}
			br.paths = append(br.paths, path)
		}
		r.bodyRename = append(r.bodyRename, br)
	}
	if alwaysParse {
		r.prefilter = nil
	}

	r.summary = fmt.Sprintf("%d header removals, %d header renames, %d body removals, %d body renames",
		len(r.headerRemove), len(r.headerRename), len(r.bodyRemove), len(r.bodyRename))
	return r, nil
}

func (r *rules) hasHeaderRules() bool { return r != nil && len(r.headerRemove)+len(r.headerRename) > 0 }
func (r *rules) hasBodyRules() bool   { return r != nil && len(r.bodyRemove)+len(r.bodyRename) > 0 }

// applyHeaders removes and renames response headers in place. Removal matches
// the original names; renames apply to the lower-cased names that survive.
func (r *rules) applyHeaders(resp *schemas.HTTPResponseMetadata) {
	names := make([]string, 0, len(resp.Headers))
	for name := range resp.Headers {
		names = append(names, name)
	}
	for _, name := range names {
		value, ok := resp.Headers[name]
		if !ok {
			continue // dropped by an earlier rename onto the same name
		}
		lower := strings.ToLower(name)
		if r.removesHeader(lower) {
			delete(resp.Headers, name)
			continue
		}
		newName, newValue := lower, value
		for _, rule := range r.headerRename {
			if rule.name != nil {
				newName = rule.name.ReplaceAllString(newName, rule.replacement)
			}
			if rule.value != nil {
				newValue = rule.value.ReplaceAllString(newValue, rule.replacement)
			}
		}
		if newName == lower && newValue == value {
			continue
		}
		delete(resp.Headers, name)
		if newName = strings.TrimSpace(newName); newName != "" {
			resp.SetHeader(newName, newValue)
		}
	}
}

func (r *rules) removesHeader(lowerName string) bool {
	for _, re := range r.headerRemove {
		if re.MatchString(lowerName) {
			return true
		}
	}
	return false
}

// mightApply is a cheap check on the raw body: false means no body rule can
// match, so the body need not be parsed.
func (r *rules) mightApply(raw []byte) bool {
	for _, br := range r.bodyRename {
		if br.re.Match(raw) {
			return true
		}
	}
	if len(r.bodyRemove) == 0 {
		return false
	}
	if r.prefilter == nil {
		return true
	}
	for _, key := range r.prefilter {
		if bytes.Contains(raw, key) {
			return true
		}
	}
	return false
}

// sanitizeJSON applies the body rules to a JSON document: removals first, so
// they name keys as Bifrost writes them, then renames in order. It reports
// whether anything changed; unchanged input is returned as is.
func (r *rules) sanitizeJSON(raw []byte) ([]byte, bool, error) {
	if !r.hasBodyRules() || !r.mightApply(raw) {
		return raw, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep numbers exactly as written
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return raw, false, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return raw, false, errors.New("trailing data after JSON document")
	}

	changed := false
	for _, path := range r.bodyRemove {
		changed = removeAt(doc, path) || changed
	}
	for _, rule := range r.bodyRename {
		fn := func(node any) (any, bool) { return renameKeys(node, rule.re, rule.replacement) }
		if rule.values {
			fn = func(node any) (any, bool) { return replaceStrings(node, rule.re, rule.replacement) }
		}
		if rule.paths == nil {
			var ok bool
			doc, ok = fn(doc)
			changed = ok || changed
			continue
		}
		for _, path := range rule.paths {
			var ok bool
			doc, ok = transformAt(doc, path, fn)
			changed = ok || changed
		}
	}
	if !changed {
		return raw, false, nil
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // keep <, > and & as Bifrost wrote them
	if err := enc.Encode(doc); err != nil {
		return raw, false, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), true, nil
}

// removeAt deletes what path names below node, whatever its type.
func removeAt(node any, path jsonPath) bool {
	switch n := node.(type) {
	case []any:
		changed := false
		for _, el := range n {
			changed = removeAt(el, path) || changed
		}
		return changed
	case map[string]any:
		seg := path[0]
		if len(path) == 1 {
			if seg == "*" {
				changed := len(n) > 0
				clear(n)
				return changed
			}
			if _, ok := n[seg]; ok {
				delete(n, seg)
				return true
			}
			return false
		}
		if seg == "*" {
			changed := false
			for _, v := range n {
				changed = removeAt(v, path[1:]) || changed
			}
			return changed
		}
		if child, ok := n[seg]; ok {
			return removeAt(child, path[1:])
		}
	}
	return false
}

// transformAt applies fn to every node path names below node, returning the
// possibly replaced node.
func transformAt(node any, path jsonPath, fn func(any) (any, bool)) (any, bool) {
	if len(path) == 0 {
		return fn(node)
	}
	switch n := node.(type) {
	case []any:
		changed := false
		for i, el := range n {
			if nv, ok := transformAt(el, path, fn); ok {
				n[i], changed = nv, true
			}
		}
		return n, changed
	case map[string]any:
		changed := false
		visit := func(k string) {
			if nv, ok := transformAt(n[k], path[1:], fn); ok {
				n[k], changed = nv, true
			}
		}
		if path[0] == "*" {
			for k := range n {
				visit(k)
			}
		} else if _, ok := n[path[0]]; ok {
			visit(path[0])
		}
		return n, changed
	}
	return node, false
}

// renameKeys rewrites object keys at every depth below node.
func renameKeys(node any, re *regexp.Regexp, replacement string) (any, bool) {
	changed := false
	switch n := node.(type) {
	case []any:
		for i, el := range n {
			if nv, ok := renameKeys(el, re, replacement); ok {
				n[i], changed = nv, true
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		for _, k := range keys {
			v := n[k]
			if nv, ok := renameKeys(v, re, replacement); ok {
				v, changed = nv, true
			}
			nk := re.ReplaceAllString(k, replacement)
			if nk != k {
				delete(n, k)
				changed = true
			}
			n[nk] = v
		}
	}
	return node, changed
}

// replaceStrings rewrites every string value at or below node.
func replaceStrings(node any, re *regexp.Regexp, replacement string) (any, bool) {
	switch n := node.(type) {
	case string:
		out := re.ReplaceAllString(n, replacement)
		return out, out != n
	case []any:
		changed := false
		for i, el := range n {
			if nv, ok := replaceStrings(el, re, replacement); ok {
				n[i], changed = nv, true
			}
		}
		return n, changed
	case map[string]any:
		changed := false
		for k, v := range n {
			if nv, ok := replaceStrings(v, re, replacement); ok {
				n[k], changed = nv, true
			}
		}
		return n, changed
	}
	return node, false
}
