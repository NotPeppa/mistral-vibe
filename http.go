package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// apiClient reaches Mistral and CPA's own management API. Go does not read the
// OS proxy settings, so a machine that reaches Mistral only through a local
// proxy would fail the TLS handshake without this.
func apiClient() *http.Client {
	return &http.Client{Transport: planTransport(), Timeout: 0}
}

// planTransport honours the proxy the operator configured, but keeps loopback
// and private origins direct: those are CPA itself or a LAN deployment, which a
// public-facing proxy cannot resolve.
func planTransport() *http.Transport {
	proxyURL := firstNonEmpty(
		os.Getenv("VIBE_PLAN_PROXY"),
		os.Getenv("HTTPS_PROXY"),
		os.Getenv("https_proxy"),
		os.Getenv("ALL_PROXY"),
		os.Getenv("all_proxy"),
	)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConnsPerHost = 4
	transport.ResponseHeaderTimeout = 30 * time.Second
	if proxyURL == "" {
		return transport
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return transport
	}
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		if isLocalOrigin(req.URL.Hostname()) {
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

// insecureTransport is used only by tests that point at a self-signed server.
func insecureTransport() *http.Transport {
	transport := planTransport()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test only
	return transport
}
