package audit

// An admin audit row's details must render inside admin_activity's 4096-byte
// CHECK (migration 0028). boundedDetails refuses a payload over it, and
// TryRecord then only LOGS the refusal, so an over-long row is silently lost
// (erratum 25 on quasar-protocol#29). These caps keep a row that repeats ids or
// agent-reported text well inside the bound, and each one records that it had
// to cut, so a reader can tell a complete list from a shortened one.
const (
	// MaxListIDs is the most entries one details list carries.
	MaxListIDs = 40
	// MaxListBytes bounds what those entries weigh together. 40 ids of a fixed
	// width (a UUID is 36 bytes, so ~1.5 KiB) are already under it; a list of
	// NAMES is not fixed-width, so the count alone would not bound the row and
	// this budget does.
	MaxListBytes = 2048
	// MaxTextBytes bounds one free-text field. It counts BYTES, like the CHECK,
	// so a cut lands on a UTF-8 boundary rather than mid-rune.
	MaxTextBytes = 512
)

// CapIDs sets details[key] to ids, cut to MaxListIDs entries and to
// MaxListBytes of them, with key+"_truncated" when either cut happened. Only
// whole entries are kept: half a name would read as a different one.
//
// A nil list is written as [] rather than null, so a reader can index it
// without a guard. The caller records the true count itself, since only it
// knows what the list is.
func CapIDs(details map[string]any, key string, ids []string) {
	if ids == nil {
		ids = []string{}
	}
	if len(ids) > MaxListIDs {
		ids, details[key+"_truncated"] = ids[:MaxListIDs], true
	}
	for i, size := 0, 0; i < len(ids); i++ {
		size += len(ids[i]) + 3 // the quotes and the comma each entry costs
		if size > MaxListBytes {
			ids, details[key+"_truncated"] = ids[:i], true
			break
		}
	}
	details[key] = ids
}

// CapText sets details[key] to v, cut at MaxTextBytes with key+"_truncated"
// when it had to cut. Only a string is bounded; anything else is written as
// reported, because its size is the emitter's own business.
func CapText(details map[string]any, key string, v any) {
	s, ok := v.(string)
	if !ok {
		details[key] = v
		return
	}
	if len(s) > MaxTextBytes {
		s, details[key+"_truncated"] = TruncateBytes(s, MaxTextBytes), true
	}
	details[key] = s
}
