//go:build race

package httpx_test

// raceBuild is true when this test binary was built with -race. The race
// detector's shadow-memory instrumentation inflates every allocation, which
// makes an allocation-bytes assertion like TestCompressPoolBoundsAllocations
// PerRequest meaningless (and flaky) under it — see that test for why it
// skips when this is true.
const raceBuild = true
