package library

import "strings"

// Failure classes of a scan, the only description of a failure that may reach a log. The
// node agent labels its own failures with the same strings (library_scan.rs, ScanError);
// testdata/library/scan-failure-classes.json pins the two together.
const (
	classUnclassified    = "unclassified; see the scan row"
	classNoHomeRoot      = "home root not configured"
	classRootOutside     = "root outside the home root"
	classRootNotDir      = "root is not a directory"
	classTimedOut        = "walk timed out"
	classUnreadableOther = "path unreadable (other)"
	classOverCap         = "manifest over the size cap"
	classNotUTF8         = "manifest not valid UTF-8"
	classUnparsable      = "manifest unparsable"
)

// unreadableKinds are the io error kinds a class may name; any other is "other".
var unreadableKinds = map[string]bool{
	"PermissionDenied": true, "NotADirectory": true, "IsADirectory": true, "InvalidData": true,
	"Interrupted": true, "TimedOut": true, "ResourceBusy": true, "ReadOnlyFilesystem": true,
	"StorageFull": true,
}

// scanFailureClass maps the error text of a failed scan report to its class. The text names
// paths, games and Steam accounts, so the result is always one of the fixed classes above
// (or "path unreadable (<allow-listed kind>)"), never a part of msg.
func scanFailureClass(msg string) string {
	switch msg {
	case "agent has no configured home root":
		return classNoHomeRoot
	case "root_path outside configured home root":
		return classRootOutside
	case "root_path is not a real directory":
		return classRootNotDir
	}
	switch {
	case strings.HasPrefix(msg, "library scan timed out after "):
		return classTimedOut
	case strings.HasPrefix(msg, "reading "):
		if i := strings.LastIndex(msg, " failed: "); i >= 0 {
			if kind := msg[i+len(" failed: "):]; unreadableKinds[kind] {
				return "path unreadable (" + kind + ")"
			}
			return classUnreadableOther
		}
	case strings.HasPrefix(msg, "manifest "):
		switch {
		case strings.HasSuffix(msg, " is not valid UTF-8"):
			return classNotUTF8
		case strings.HasSuffix(msg, " failed to parse (malformed or incomplete)"):
			return classUnparsable
		case strings.HasSuffix(msg, " byte cap") && strings.Contains(msg, " bytes, over the "):
			return classOverCap
		}
	}
	return classUnclassified
}
