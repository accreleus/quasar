package httpx

import "net/http"

// Router is the route-registration surface the handlers' Register methods need;
// *http.ServeMux satisfies it.
//
// It is an interface only so TestOpenAPIDrift can pass a recording
// implementation and capture every registered (method, pattern) pair —
// http.ServeMux does not expose its patterns. Every /v1 route must appear in
// protocol/openapi.yaml and vice-versa.
type Router interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}
