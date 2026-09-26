// Package prerh06 is a test fixture: the release-detection readers of a control plane
// that predates RH06, copied verbatim from v0.3.0's internal/platform (manifest.go,
// github.go, edge.go, source.go) with only the package clause changed. Tests hand it
// what an RH06-era release and edge build publish and assert it finds nothing
// (#365 acceptance). Never edit these files to make a test pass: they are what is
// installed in the field.
package prerh06
