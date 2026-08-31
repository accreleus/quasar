package telemetry

import "encoding/json"

// hiddenFlagKeys are the two spellings the same fact arrives under: the periodic
// browser sample carries it in the numeric metrics dict as `is_hidden` (0/1,
// because that dict is numbers), and the discrete `client.visibility_changed`
// trace event carries it in its payload as `hidden` (a JSON boolean). They mean
// exactly the same thing, and reading them with two different decoders is how one
// path came to accept only numbers and the other only booleans.
var hiddenFlagKeys = []string{"is_hidden", "hidden"}

// HiddenFlag is THE reader for "was the client tab hidden", whichever of the two
// encodings the value arrived in. It accepts a JSON boolean and a 0/1 number
// (and any non-zero number as true), from either key.
//
// present is false when the object carries neither key, or the value is neither a
// number nor a boolean — "we were not told" is a distinct answer from "not
// hidden", and the taxonomy depends on the difference.
func HiddenFlag(raw json.RawMessage) (hidden bool, present bool) {
	if len(raw) == 0 {
		return false, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false, false
	}
	for _, k := range hiddenFlagKeys {
		v, ok := obj[k]
		if !ok {
			continue
		}
		var b bool
		if err := json.Unmarshal(v, &b); err == nil {
			return b, true
		}
		var f float64
		if err := json.Unmarshal(v, &f); err == nil {
			return f != 0, true
		}
	}
	return false, false
}
