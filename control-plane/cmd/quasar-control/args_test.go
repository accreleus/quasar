package main

import (
	"bytes"
	"strings"
	"testing"
)

// #515: no argument may reach run(), which boots against the database.
func TestEveryArgumentIsAnsweredWithoutStarting(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"--version"}, 0, "quasar-control "},
		{[]string{"version"}, 0, "quasar-control "},
		{[]string{"--help"}, 2, ""},
		{[]string{"--version", "extra"}, 2, ""},
		{[]string{"migrate"}, 2, ""},
	} {
		var out, errOut bytes.Buffer
		if code := refuseArgs(tc.args, &out, &errOut); code != tc.code || !strings.HasPrefix(out.String(), tc.out) || (tc.code != 0) != (errOut.Len() > 0) {
			t.Errorf("%q: code=%d stdout=%q stderr=%q", tc.args, code, out.String(), errOut.String())
		}
	}
}
