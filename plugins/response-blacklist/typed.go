package main

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// Stream chunks reach plugins as Bifrost's typed values, and the transport
// serializes or converts whatever the hook returns. Re-decoding a cleaned JSON
// copy would drop the fields Bifrost keeps out of JSON (json:"-") that the
// integration converters still read, so the body rules are applied to the
// typed value instead: every struct, map and slice on a rule's path is copied
// before it is changed, so the chunk Bifrost holds is never modified.
//
// Removing a struct field sets it to its zero value, which drops it from the
// output when its JSON tag says omitempty. Key renames cannot be expressed in
// a typed value and are skipped for streams.

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// sanitizeChunk returns a cleaned copy of chunk, or chunk itself when no rule
// applies. unremoved lists removal paths still present in the output.
func (r *rules) sanitizeChunk(chunk *schemas.BifrostStreamChunk) (out *schemas.BifrostStreamChunk, unremoved []string) {
	raw, err := json.Marshal(chunk)
	if err != nil || !r.mightApply(raw) {
		return chunk, nil
	}

	cp := *chunk
	outer := reflect.ValueOf(&cp).Elem()
	changed := false
	for i := 0; i < outer.NumField(); i++ {
		inner := outer.Field(i) // one embedded pointer per kind of chunk
		if inner.Kind() != reflect.Pointer || inner.IsNil() {
			continue
		}
		v := inner
		for _, path := range r.bodyRemove {
			if nv, ok := walkTyped(v, path, true, zeroLeaf); ok {
				v, changed = nv, true
			}
		}
		for _, rule := range r.bodyRename {
			if !rule.values {
				continue
			}
			leaf := func(v reflect.Value) (reflect.Value, bool) { return replaceTyped(v, rule.re, rule.replacement) }
			for _, path := range rule.paths {
				if nv, ok := walkTyped(v, path, false, leaf); ok {
					v, changed = nv, true
				}
			}
		}
		inner.Set(v)
		break
	}
	if !changed {
		return chunk, nil
	}

	// Report what the typed edit could not take out (a field without omitempty).
	if check, err := json.Marshal(&cp); err == nil {
		var doc any
		if json.Unmarshal(check, &doc) == nil {
			for _, path := range r.bodyRemove {
				if removeAt(doc, path) {
					unremoved = append(unremoved, path.String())
				}
			}
		}
	}
	return &cp, unremoved
}

func zeroLeaf(v reflect.Value) (reflect.Value, bool) {
	if v.IsZero() {
		return v, false
	}
	return reflect.Zero(v.Type()), true
}

// walkTyped applies leaf to whatever path names below v and returns a copy of
// v with the result, or v itself when nothing changed. With remove set, a
// path ending at a map key deletes the key rather than handing it to leaf.
func walkTyped(v reflect.Value, path jsonPath, remove bool, leaf func(reflect.Value) (reflect.Value, bool)) (reflect.Value, bool) {
	if len(path) == 0 {
		return leaf(v)
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v, false
		}
		ne, ok := walkTyped(v.Elem(), path, remove, leaf)
		if !ok {
			return v, false
		}
		np := reflect.New(v.Type().Elem())
		np.Elem().Set(ne)
		return np, true

	case reflect.Interface:
		if v.IsNil() {
			return v, false
		}
		ne, ok := walkTyped(v.Elem(), path, remove, leaf)
		if !ok {
			return v, false
		}
		ni := reflect.New(v.Type()).Elem()
		ni.Set(ne)
		return ni, true

	case reflect.Struct:
		t := v.Type()
		var cp reflect.Value
		set := func(i int, nv reflect.Value) {
			if !cp.IsValid() {
				cp = reflect.New(t).Elem()
				cp.Set(v)
			}
			cp.Field(i).Set(nv)
		}
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			name, promoted, ok := jsonField(sf)
			if !ok || !sf.IsExported() { // an unexported field cannot be set on the copy
				continue
			}
			if promoted { // embedded struct: its fields belong to this object
				if nv, ok := walkTyped(v.Field(i), path, remove, leaf); ok {
					set(i, nv)
				}
				continue
			}
			if path[0] != "*" && path[0] != name {
				continue
			}
			if nv, ok := walkTyped(v.Field(i), path[1:], remove, leaf); ok {
				set(i, nv)
			}
		}
		if !cp.IsValid() {
			return v, false
		}
		return cp, true

	case reflect.Map:
		if v.IsNil() || v.Type().Key().Kind() != reflect.String {
			return v, false
		}
		var cp reflect.Value
		ensure := func() {
			if !cp.IsValid() {
				cp = reflect.MakeMapWithSize(v.Type(), v.Len())
				iter := v.MapRange()
				for iter.Next() {
					cp.SetMapIndex(iter.Key(), iter.Value())
				}
			}
		}
		iter := v.MapRange()
		for iter.Next() {
			k := iter.Key()
			if path[0] != "*" && k.String() != path[0] {
				continue
			}
			if remove && len(path) == 1 {
				ensure()
				cp.SetMapIndex(k, reflect.Value{}) // delete
				continue
			}
			if nv, ok := walkTyped(iter.Value(), path[1:], remove, leaf); ok {
				ensure()
				cp.SetMapIndex(k, nv)
			}
		}
		if !cp.IsValid() {
			return v, false
		}
		return cp, true

	case reflect.Slice:
		if v.Type() == rawMessageType {
			return walkRawJSON(v, path, remove, leaf)
		}
		if v.IsNil() || v.Type().Elem().Kind() == reflect.Uint8 {
			return v, false // a nil slice, or bytes
		}
		var cp reflect.Value
		for i := 0; i < v.Len(); i++ {
			if nv, ok := walkTyped(v.Index(i), path, remove, leaf); ok { // arrays are transparent
				if !cp.IsValid() {
					cp = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
					reflect.Copy(cp, v)
				}
				cp.Index(i).Set(nv)
			}
		}
		if !cp.IsValid() {
			return v, false
		}
		return cp, true
	}
	return v, false
}

// walkRawJSON continues a path into a json.RawMessage, which Bifrost passes
// through untouched, by editing its JSON.
func walkRawJSON(v reflect.Value, path jsonPath, remove bool, leaf func(reflect.Value) (reflect.Value, bool)) (reflect.Value, bool) {
	var doc any
	if len(v.Bytes()) == 0 || json.Unmarshal(v.Bytes(), &doc) != nil {
		return v, false
	}
	changed := false
	if remove {
		changed = removeAt(doc, path)
	} else {
		doc, changed = transformAt(doc, path, func(node any) (any, bool) {
			nv, ok := leaf(reflect.ValueOf(&node).Elem())
			if !ok {
				return node, false
			}
			return nv.Interface(), true
		})
	}
	if !changed {
		return v, false
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return v, false
	}
	return reflect.ValueOf(json.RawMessage(out)), true
}

// replaceTyped rewrites every string at or below v, copying what it changes.
func replaceTyped(v reflect.Value, re *regexp.Regexp, replacement string) (reflect.Value, bool) {
	switch v.Kind() {
	case reflect.String:
		out := re.ReplaceAllString(v.String(), replacement)
		if out == v.String() {
			return v, false
		}
		nv := reflect.New(v.Type()).Elem()
		nv.SetString(out)
		return nv, true
	case reflect.Pointer, reflect.Interface, reflect.Struct, reflect.Map, reflect.Slice:
		// Walking with a wildcard visits every child: reuse walkTyped's copying.
		if v.Kind() == reflect.Slice && v.Type() == rawMessageType {
			return walkRawJSON(v, jsonPath{"*"}, false, func(n reflect.Value) (reflect.Value, bool) { return replaceTyped(n, re, replacement) })
		}
		leaf := func(n reflect.Value) (reflect.Value, bool) { return replaceTyped(n, re, replacement) }
		if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
			if v.IsNil() {
				return v, false
			}
			ne, ok := replaceTyped(v.Elem(), re, replacement)
			if !ok {
				return v, false
			}
			if v.Kind() == reflect.Pointer {
				np := reflect.New(v.Type().Elem())
				np.Elem().Set(ne)
				return np, true
			}
			ni := reflect.New(v.Type()).Elem()
			ni.Set(ne)
			return ni, true
		}
		if v.Kind() == reflect.Slice {
			var cp reflect.Value
			for i := 0; i < v.Len(); i++ {
				if nv, ok := replaceTyped(v.Index(i), re, replacement); ok {
					if !cp.IsValid() {
						cp = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
						reflect.Copy(cp, v)
					}
					cp.Index(i).Set(nv)
				}
			}
			if !cp.IsValid() {
				return v, false
			}
			return cp, true
		}
		return walkTyped(v, jsonPath{"*"}, false, leaf)
	}
	return v, false
}

// jsonField reports the JSON name encoding/json gives a struct field, whether
// it is an embedded struct whose fields are promoted, and whether it is
// serialized at all.
func jsonField(sf reflect.StructField) (name string, promoted, ok bool) {
	tag := sf.Tag.Get("json")
	if tag == "-" {
		return "", false, false
	}
	name, _, _ = strings.Cut(tag, ",")
	if sf.Anonymous && name == "" {
		t := sf.Type
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() == reflect.Struct {
			return "", true, true
		}
	}
	if !sf.IsExported() {
		return "", false, false
	}
	if name == "" {
		name = sf.Name
	}
	return name, false, true
}
