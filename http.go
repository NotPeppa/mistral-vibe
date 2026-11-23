package main

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// sharedTransport reuses a single connection pool across all outbound requests
// instead of allocating a new Transport (and leaking idle connections) per call.
// Proxy environment variables are resolved dynamically per request so runtime
// or test overrides take effect immediately while local origins stay direct.
var (
	sharedTransport = planTransport()
	sharedHTTP      = &http.Client{Transport: sharedTransport, Timeout: httpTimeout}
)

// apiClient reaches Mistral and CPA's own management API with a shared
// transport and bounded timeout.
func apiClient() *http.Client {
	return sharedHTTP
}

// planTransport honours the proxy the operator configured, but keeps loopback
// and private origins direct: those are CPA itself or a LAN deployment, which a
// public-facing proxy cannot resolve.
func planTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 4
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		if req != nil && req.URL != nil && isLocalOrigin(req.URL.Hostname()) {
			return nil, nil
		}
		proxyURL := firstNonEmpty(
			os.Getenv("VIBE_PLAN_PROXY"),
			os.Getenv("HTTPS_PROXY"),
			os.Getenv("https_proxy"),
			os.Getenv("ALL_PROXY"),
			os.Getenv("all_proxy"),
		)
		if proxyURL == "" {
			return nil, nil
		}
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, nil
		}
		return parsed, nil
	}
	return transport
}

// isLocalOrigin reports loopback, link-local and RFC1918 literals (and .local).
func isLocalOrigin(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// isLoopbackOrigin reports whether host is strictly a loopback address.
func isLoopbackOrigin(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
