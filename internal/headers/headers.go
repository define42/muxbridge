package headers

import (
	"net/http"
	"strings"

	"github.com/define42/muxbridge/gen/tunnelpb"
)

// hopByHopHeaders are connection-level headers that MUST NOT be forwarded by
// an HTTP proxy, per RFC 7230 §6.1. "Connection" and "Upgrade" are handled
// separately so that WebSocket / h2c upgrades still work end-to-end.
var hopByHopHeaders = map[string]struct{}{
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
}

// isHopByHop reports whether a header is a hop-by-hop header that must be
// stripped when forwarding through a proxy. Any header listed in
// connectionTokens (values named in a Connection header) is also stripped.
func isHopByHop(key string, connectionTokens map[string]struct{}) bool {
	lower := strings.ToLower(key)
	if _, ok := hopByHopHeaders[lower]; ok {
		return true
	}
	if connectionTokens != nil {
		if _, ok := connectionTokens[lower]; ok {
			return true
		}
	}
	return false
}

// connectionTokens parses the Connection header values and returns a set of
// lowercased header names that must be treated as hop-by-hop. The "upgrade"
// token itself is not returned: it must be preserved so the downstream peer
// can observe protocol upgrade negotiation.
func connectionTokens(h http.Header) map[string]struct{} {
	values := h.Values("Connection")
	if len(values) == 0 {
		return nil
	}
	tokens := make(map[string]struct{})
	for _, v := range values {
		for _, tok := range strings.Split(v, ",") {
			tok = strings.ToLower(strings.TrimSpace(tok))
			if tok == "" || tok == "upgrade" {
				continue
			}
			tokens[tok] = struct{}{}
		}
	}
	return tokens
}

// hasUpgrade reports whether the header set is negotiating an upgrade.
func hasUpgrade(h http.Header) bool {
	return h.Get("Upgrade") != ""
}

// ToProto converts an http.Header to the wire representation, stripping
// hop-by-hop headers so they are not forwarded across the tunnel. The
// Connection and Upgrade headers are preserved only when an upgrade is in
// progress (Upgrade header present); otherwise they are stripped.
func ToProto(h http.Header) []*tunnelpb.Header {
	connTokens := connectionTokens(h)
	upgrading := hasUpgrade(h)
	out := make([]*tunnelpb.Header, 0, len(h))
	for k, v := range h {
		if shouldStrip(k, connTokens, upgrading) {
			continue
		}
		vals := append([]string(nil), v...)
		out = append(out, &tunnelpb.Header{Key: k, Values: vals})
	}
	return out
}

// FromProto converts wire headers back into an http.Header. Hop-by-hop
// headers that slipped through (e.g., from a misbehaving backend) are
// stripped defensively, while preserving upgrade negotiation.
func FromProto(in []*tunnelpb.Header) http.Header {
	h := make(http.Header, len(in))
	for _, kv := range in {
		if kv == nil {
			continue
		}
		vals := append([]string(nil), kv.Values...)
		h[kv.Key] = vals
	}
	connTokens := connectionTokens(h)
	upgrading := hasUpgrade(h)
	for k := range h {
		if shouldStrip(k, connTokens, upgrading) {
			delete(h, k)
		}
	}
	return h
}

func shouldStrip(key string, connTokens map[string]struct{}, upgrading bool) bool {
	lower := strings.ToLower(key)
	if lower == "connection" || lower == "upgrade" {
		return !upgrading
	}
	return isHopByHop(lower, connTokens)
}

// Copy copies headers from src to dst without filtering. Hop-by-hop headers
// should already have been stripped by ToProto/FromProto before reaching
// here, so Copy is safe to use for forwarding responses to the browser.
func Copy(dst, src http.Header) {
	for k, v := range src {
		for _, vv := range v {
			dst.Add(k, vv)
		}
	}
}
