package library

import "testing"

func TestRedactScanError(t *testing.T) {
	const root = "/homes/opaque-id"
	for _, c := range []struct{ name, in, root, want string }{
		{"manifest name with an appid", "manifest /homes/opaque-id/.local/share/Steam/steamapps/appmanifest_730.acf is not valid UTF-8", root,
			"manifest .local/share/Steam/steamapps/appmanifest_<appid>.acf is not valid UTF-8"},
		{"nested library, trailing-slash root", "manifest /homes/opaque-id/games/SteamLibrary/steamapps/appmanifest_1086940.acf failed to parse", root + "/",
			"manifest games/SteamLibrary/steamapps/appmanifest_<appid>.acf failed to parse"},
		{"path with no appid", "reading /homes/opaque-id/games/steamapps failed: PermissionDenied", root,
			"reading games/steamapps failed: PermissionDenied"},
		{"the root itself", "reading /homes/opaque-id failed: PermissionDenied", root, "reading . failed: PermissionDenied"},
		{"error with no path", "library scan timed out after 5s", root, "library scan timed out after 5s"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := redactScanError(c.in, c.root); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
