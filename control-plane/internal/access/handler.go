package access

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/origins"
)

// maxReflectedLength caps every request-derived string echoed back (Host,
// Origin, forwarded proto) — attacker-controllable headers rendered in an
// admin UI (§S6b). Not the escaping control: React escapes the JSON values,
// and this route must never be consumed with dangerouslySetInnerHTML.
const maxReflectedLength = 256

// Service serves the §S6 access surface.
type Service struct {
	manager *Manager
	log     *slog.Logger

	// resolver is the same instance internal/signal enforces with: this endpoint
	// presents a Decision rather than computing its own, so diagnostic and
	// enforcer cannot disagree. nil is legal (route recorder); the origins
	// section then reports nothing.
	resolver *origins.Resolver
}

// NewService builds the handler. Every dependency may be nil for the route
// recorder; each request path checks what it needs.
func NewService(manager *Manager, resolver *origins.Resolver, log *slog.Logger) *Service {
	return &Service{manager: manager, resolver: resolver, log: log}
}

// Register wires the routes. The auth split (control-api.md §Authorization):
// the certificate download is unauthenticated on purpose — a client that does
// not yet trust the certificate often cannot log in to fetch it, and it only
// discloses what every TLS handshake already transmits. access-check goes
// through the same RequireAuth → RequireAdmin gate as every admin route.
func (s *Service) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.HandleFunc("GET /v1/tls/certificate.pem", s.handleCertificateDownload)
	mux.Handle("GET /v1/admin/access-check", admin(http.HandlerFunc(s.handleAccessCheck)))
}

// --- S6a: public certificate download ----------------------------------------

// handleCertificateDownload serves the leaf certificate currently in force,
// via Info.LeafPEM — no file path, no key variable in scope, so a private key
// is not declined but unrepresentable. Guarded by
// TestCertificateDownloadNeverLeaksKey (§S6a).
func (s *Service) handleCertificateDownload(w http.ResponseWriter, r *http.Request) {
	if s.manager == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeInternal,
			"TLS is not enabled on this control plane, so there is no certificate to download")
		return
	}
	info := s.manager.Current()
	body := info.LeafPEM()
	if len(body) == 0 {
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeInternal,
			"no certificate is loaded")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="quasar-control-plane.pem"`)
	// An upload (§S6d) can replace the certificate at any moment; a cached stale
	// copy is what makes an operator trust the wrong fingerprint.
	w.Header().Set("Cache-Control", "no-store")
	// Fingerprint as a header so `curl -I` is a complete verification path.
	w.Header().Set("X-Quasar-Certificate-Fingerprint", info.FingerprintSHA256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// --- S6b: access self-check ---------------------------------------------------

type accessCheckRequestInfo struct {
	Host   string `json:"host"`
	Origin string `json:"origin"`
	// Scheme is what this control plane saw on the wire — observed, never taken
	// from a header.
	Scheme string `json:"scheme"`
	// ForwardedProto is what an upstream proxy claims the browser used. A claim,
	// not a fact — see detectForwardedProto.
	ForwardedProto string `json:"forwarded_proto,omitempty"`
	// TLSTerminatedUpstream is the §S6-0 topology-C detection.
	TLSTerminatedUpstream bool `json:"tls_terminated_upstream"`
	// SecureContext reports whether the browser will treat the page as a secure
	// context — the precondition for the microphone (§S7) and for Keyboard Lock.
	SecureContext bool `json:"secure_context"`
}

type accessCheckCertificate struct {
	// InUse is false under topology C (§S6-0): when an upstream proxy terminates
	// TLS the browser never sees this certificate, and telling the operator to
	// fix its SANs is the failure this endpoint exists to prevent.
	InUse          bool   `json:"in_use"`
	NotInUseReason string `json:"not_in_use_reason,omitempty"`
	Info           *Info  `json:"info,omitempty"`
	HostCovered    *bool  `json:"host_covered,omitempty"`
	Advice         string `json:"advice,omitempty"`
}

type accessCheckOrigins struct {
	// Configured reports whether any allow-list entry is in force. False is a
	// normal, working state — see Advice.
	Configured bool `json:"configured"`
	// Source: which of "environment"/"database" won, so a UI can grey out a
	// control the environment has pinned.
	Source string `json:"source"`
	// Allowed is the resolved list actually enforced by /v1/signal.
	Allowed []string `json:"allowed"`
	// RequestOriginAllowed is whether this request's Origin would pass; null
	// when no Origin header was sent.
	RequestOriginAllowed *bool `json:"request_origin_allowed"`
	// SameOriginExemption: passed only because Origin and Host agree — the case
	// that silently breaks behind a Host-rewriting proxy (§S6e).
	SameOriginExemption bool   `json:"same_origin_exemption"`
	Advice              string `json:"advice,omitempty"`
	// SuggestedEntry is the current origin ready to be added when it would
	// otherwise be refused; "" when there is nothing to suggest.
	SuggestedEntry string `json:"suggested_entry,omitempty"`
}

type accessCheckResponse struct {
	Request     accessCheckRequestInfo `json:"request"`
	Certificate accessCheckCertificate `json:"certificate"`
	Origins     accessCheckOrigins     `json:"origins"`
}

// detectForwardedProto reads the protocol an upstream proxy claims the browser
// spoke (X-Forwarded-Proto, then RFC 7239 Forwarded). The result authorises
// nothing (§S6-0) — it only softens advice about a certificate the browser
// never sees. Anyone can spoof it; no access decision reads it.
func detectForwardedProto(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); v != "" {
		// A proxy chain produces a comma list; the first is nearest the browser.
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		return strings.ToLower(strings.TrimSpace(v))
	}
	// RFC 7239: Forwarded: for=...;proto=https;by=...
	for _, element := range strings.Split(r.Header.Get("Forwarded"), ",") {
		for _, param := range strings.Split(element, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(k), "proto") {
				continue
			}
			return strings.ToLower(strings.Trim(strings.TrimSpace(v), `"`))
		}
	}
	return ""
}

// truncate applies the §S6b length cap, marking that it did so.
func truncate(s string) string {
	if len(s) <= maxReflectedLength {
		return s
	}
	return s[:maxReflectedLength] + "…(truncated)"
}

func (s *Service) handleAccessCheck(w http.ResponseWriter, r *http.Request) {
	forwardedProto := detectForwardedProto(r)
	// Topology C is "a proxy is in front of us at all", not merely "the proxy
	// says https": under terminate-and-re-encrypt our SANs still are not what
	// the browser validates.
	upstreamTLS := forwardedProto != ""
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	rawOrigin := r.Header.Get("Origin")
	resp := accessCheckResponse{
		Request: accessCheckRequestInfo{
			Host:                  truncate(r.Host),
			Origin:                truncate(rawOrigin),
			Scheme:                scheme,
			ForwardedProto:        truncate(forwardedProto),
			TLSTerminatedUpstream: upstreamTLS,
			SecureContext:         secureContext(scheme, forwardedProto, rawOrigin, r.Host),
		},
	}
	resp.Certificate = s.certificateSection(r, upstreamTLS, r.TLS != nil)
	resp.Origins = s.originsSection(r.Context(), r, rawOrigin)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) certificateSection(r *http.Request, upstreamTLS, directTLS bool) accessCheckCertificate {
	if s.manager == nil {
		return accessCheckCertificate{
			InUse: false,
			NotInUseReason: "TLS is disabled on this control plane (QUASAR_TLS=off), so it serves no certificate. " +
				"That is a supported configuration when something in front of it terminates TLS.",
		}
	}
	info := s.manager.Current()
	if upstreamTLS {
		// §S6-0 topology C: say plainly that this is correct, so the operator
		// does not "fix" a working deployment.
		return accessCheckCertificate{
			InUse: false,
			NotInUseReason: "TLS is terminated by a proxy in front of this control plane, so the browser never sees " +
				"this certificate and its names do not matter. This is a supported, complete setup — the certificate " +
				"your users trust is the proxy's. Nothing to do here; check allowed origins below instead.",
			Info: &info,
		}
	}
	if !directTLS {
		// Plain HTTP with no proxy claiming TLS (supported: QUASAR_HTTP_REDIRECT
		// off). This browser never saw the certificate, so in_use=true would
		// contradict scheme=http in the same response.
		return accessCheckCertificate{
			InUse: false,
			NotInUseReason: "This request arrived over plain HTTP, so the browser never saw this control plane's " +
				"certificate — there is nothing for it to trust here. If you expect HTTPS, reach the instance on its " +
				"TLS port, or check QUASAR_HTTP_REDIRECT if you expect plain HTTP to redirect.",
			Info: &info,
		}
	}

	covered := info.CoversHost(r.Host)
	out := accessCheckCertificate{InUse: true, Info: &info, HostCovered: &covered}
	// Order is by what blocks the operator right now: not-yet-valid and expired
	// first (nothing else helps while either is true), then SAN mismatch ahead
	// of near-expiry — a cert expiring in 20 days that also misses this host is
	// blocking this browser today. All reachable: NewManager's compatibility
	// fallback keeps serving an expired/mismatched on-disk pair rather than
	// refusing to boot.
	now := time.Now()
	switch {
	case now.Before(info.NotBefore):
		out.Advice = "This certificate is NOT VALID UNTIL " + info.NotBefore.Format("2006-01-02 15:04 MST") +
			", so browsers reject every connection until then. The usual cause is a wrong clock on this host or on " +
			"whatever issued the certificate — check the system time first. Nothing about trusting it or correcting " +
			"its names will help while it is post-dated."
	case now.After(info.NotAfter):
		out.Advice = "This certificate EXPIRED on " + info.NotAfter.Format("2006-01-02") + ". Browsers refuse it " +
			"outright, and trusting it or correcting its names will not help — it has to be replaced. Upload a current " +
			"certificate: for the built-in self-signed pair, delete cert.pem and key.pem from QUASAR_TLS_DIR and " +
			"recreate the container to have a fresh one generated; for a mounted pair, replace the files."
	case !covered:
		out.Advice = sanMismatchAdvice(info.Source, truncate(hostOnly(r.Host)))
	case info.DaysUntilExpiry <= 30:
		out.Advice = "This certificate expires in " + strconv.Itoa(info.DaysUntilExpiry) + " day(s). A certificate that " +
			"has to be replaced by hand becomes a silent outage on expiry — the Caddy hardened overlay renews " +
			"automatically and is the durable answer."
	case info.SelfSigned:
		out.Advice = "This certificate covers the name you used, but it is self-signed, so browsers show a warning until " +
			"it is trusted. Download it below and add it to your OS trust store — and compare its fingerprint against the " +
			"one in the control-plane startup log before you do, because downloading a certificate over a connection you " +
			"do not yet trust is only as good as that comparison. For anything reachable from the internet, a certificate " +
			"from Let's Encrypt (via the bundled Caddy overlay) is the better answer."
	}
	return out
}

// originsSection presents the shared resolver's Decision; it computes no part
// of the verdict itself (see Service.resolver).
func (s *Service) originsSection(ctx context.Context, r *http.Request, rawOrigin string) accessCheckOrigins {
	out := accessCheckOrigins{Allowed: []string{}, Source: origins.SourceDatabase}
	if s.resolver == nil {
		return out
	}
	d := s.resolver.Decide(ctx, rawOrigin, r.Host)
	out.Source = d.Source
	out.Allowed = append(out.Allowed, d.Allowlist...)
	out.Configured = len(out.Allowed) > 0
	envPinned := d.Source == origins.SourceEnvironment

	if !d.Present {
		// No Origin header: a non-browser client, which /v1/signal admits.
		out.Advice = "This request carried no Origin header, so there is nothing to evaluate. Open this check from the " +
			"browser you actually use to reach the instance to get a meaningful answer."
		return out
	}
	allowed := d.Allowed
	out.RequestOriginAllowed = &allowed
	out.SameOriginExemption = d.Exempt

	if !d.Parsed {
		out.Advice = "The browser sent an Origin this server will not parse as a plain scheme+host. That is unusual and " +
			"not something an allow-list entry can fix."
		return out
	}

	switch {
	case !d.Allowed:
		out.SuggestedEntry = d.Origin
		out.Advice = "Streaming from this address would fail: the signaling socket refuses this origin, and the browser " +
			"cannot see the 403 — it surfaces to the user as \"signaling connection failed\" with no cause. Add " +
			d.Origin + " to the allowed origins."
		if envPinned {
			out.Advice += " On this deployment the list is pinned by QUASAR_ALLOWED_ORIGINS, so it must be changed there " +
				"(and the control plane restarted) rather than in the admin UI."
			out.SuggestedEntry = ""
		}
	case d.Exempt:
		out.Advice = "This origin is accepted because it matches the Host header, not because it is listed. That exemption " +
			"disappears the moment a reverse proxy rewrites Host — which is the usual reason streaming works on the LAN " +
			"and fails through a proxy. Listing the public origin explicitly makes it robust."
	}
	return out
}

// sanMismatchAdvice is the remedy for "the certificate does not cover the name
// you used", per source: QUASAR_TLS_HOSTS regenerates nothing for a mounted
// pair, and deleting the QUASAR_TLS_DIR pems does nothing for an uploaded one
// (the database copy comes back at restart). One remedy for all sources sends
// two of the three topologies down steps that cannot work.
func sanMismatchAdvice(source, host string) string {
	head := "This certificate does not cover \"" + host + "\", so a browser reaching the instance by that name " +
		"refuses or warns — and a page that is not a secure context gets no microphone and no keyboard lock. "
	switch source {
	case SourceProvided:
		return head + "This certificate is the file mounted at QUASAR_TLS_CERT, so QUASAR_TLS_HOSTS does not apply to " +
			"it and changing that variable will do nothing. Re-issue the certificate with this name in its Subject " +
			"Alternative Names using whatever produced it, replace the mounted file, and restart the control plane."
	default: // SourceSelfSigned
		return head + "Add that name to QUASAR_TLS_HOSTS. NOTE: the self-signed certificate is generated ONCE and is " +
			"NEVER regenerated when that variable changes — so also delete cert.pem and key.pem from QUASAR_TLS_DIR and " +
			"recreate the container, or mount your own certificate at QUASAR_TLS_CERT/QUASAR_TLS_KEY instead."
	}
}

// secureContext reports whether the browser will treat its page as a secure
// context — the precondition for getUserMedia (§S7) and Keyboard Lock.
//
// The loopback exception must come from the browser's Origin, never our Host
// header: a proxy rewriting Host to its loopback backend (proxy_pass
// http://127.0.0.1:8080) would otherwise make an insecure public page report
// secure_context=true, and the operator hunts a microphone bug that is not
// there. Host is consulted only when no forwarded protocol establishes a proxy
// is in play at all (a plain navigation straight at us carries no Origin).
func secureContext(scheme, forwardedProto, rawOrigin, host string) bool {
	// The forwarded protocol, when present, overrides our own transport rather
	// than being ORed with it: a proxy speaking HTTPS to this listener while
	// serving the browser plain HTTP must not count as secure.
	if forwardedProto != "" {
		if forwardedProto == "https" {
			return true
		}
		// Not https on the browser hop; only the browser's own origin
		// (http://localhost) can still make the page secure.
		return loopbackOrigin(rawOrigin)
	}
	if scheme == "https" {
		return true
	}
	if normalized, ok := origins.Normalize(rawOrigin); ok {
		_, authority, _ := strings.Cut(normalized, "://")
		return isLoopbackAuthority(authority)
	}
	// No Origin, no proxy: a plain navigation straight to us, so our own Host is
	// the browser's authority.
	return isLoopbackAuthority(host)
}

// loopbackOrigin reports whether the browser's own Origin is a loopback
// authority. False for an absent Origin: with a proxy in play the Host is the
// backend's, so "no Origin" means no evidence, not consent.
func loopbackOrigin(rawOrigin string) bool {
	normalized, ok := origins.Normalize(rawOrigin)
	if !ok {
		return false
	}
	_, authority, _ := strings.Cut(normalized, "://")
	return isLoopbackAuthority(authority)
}

// isLoopbackAuthority reports whether a host[:port] authority is a loopback
// name/address, which browsers treat as a secure context regardless of scheme.
func isLoopbackAuthority(authority string) bool {
	h := hostOnly(authority)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// --- the secure-transport gate -----------------------------------------------
