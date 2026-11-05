package controllers

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// newWebhookClient returns an HTTP client for user-supplied alert webhook URLs.
// The URL comes from a CR spec, so the operator (which has cluster-wide
// credentials and network position) must not be steerable at the node metadata
// service or its own loopback: it refuses http(s)-less URLs, refuses to dial
// loopback/link-local/unspecified addresses (checked after DNS resolution, so
// rebinding and redirects cannot get around it) and does not follow redirects.
// Private (in-cluster) addresses stay allowed because in-cluster receivers are
// the normal case.
func newWebhookClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && webhookIPBlocked(ip) {
				return fmt.Errorf("webhook address %s is not allowed", host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func webhookIPBlocked(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// validateWebhookURL accepts only absolute http/https URLs with a host.
func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid webhook URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("webhook URL scheme must be http or https, got %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("webhook URL has no host")
	}
	return nil
}
