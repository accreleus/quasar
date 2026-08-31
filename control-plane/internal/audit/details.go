package audit

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// ChangedKeys reports the sorted wire names of the fields a request actually
// carried — the `{"keys": [...]}` idiom hostcfg/handler.go uses. Field names
// only, never values: that keeps a payload under admin_activity's 4096-byte
// CHECK (migration 0028) and keeps a secret in a request body out of the log.
//
// A field counts as carried when it is a non-nil pointer or non-empty
// json.RawMessage (the repo's PATCH structs are all-pointer so "absent" is
// distinguishable from "zero"). ignore names decoded-but-never-applied fields
// (a body `id` echoed by clients but taken from the path). v may be a struct
// or pointer to one; anything else yields nil.
func ChangedKeys(v any, ignore ...string) []string {
	skip := make(map[string]struct{}, len(ignore))
	for _, name := range ignore {
		skip[name] = struct{}{}
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	rt := rv.Type()
	keys := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		name := wireName(f)
		if name == "" {
			continue
		}
		if _, skipped := skip[name]; skipped {
			continue
		}
		if fieldCarried(rv.Field(i)) {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	return keys
}

// wireName resolves a field's JSON name, honouring `json:"-"` and options.
func wireName(f reflect.StructField) string {
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		return f.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return ""
	}
	if name == "" {
		return f.Name
	}
	return name
}

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

func fieldCarried(fv reflect.Value) bool {
	if fv.Type() == rawMessageType {
		return fv.Len() > 0
	}
	switch fv.Kind() {
	case reflect.Pointer, reflect.Interface:
		return !fv.IsNil()
	case reflect.Slice, reflect.Map:
		// A non-pointer slice/map cannot express "absent" vs "empty", so treat
		// only a non-nil, non-empty value as carried.
		return !fv.IsNil() && fv.Len() > 0
	default:
		// A plain value field carries no absent/present signal at all. Counting
		// it would produce exactly the misleading key list this avoids.
		return false
	}
}
