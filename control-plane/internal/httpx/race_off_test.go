//go:build !race

package httpx_test

// raceBuild is false for a normal (non -race) test binary. See race_on_test.go.
const raceBuild = false
