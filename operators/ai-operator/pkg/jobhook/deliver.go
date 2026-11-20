// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package jobhook delivers GryviaJobHook webhooks.
//
// Hook URLs are written by tenants, so the dialer refuses loopback, private (RFC 1918, fc00::/7), CGNAT, link-local
// (including 169.254.169.254), multicast, unspecified and reserved addresses unless they fall in an operator-allowed
// CIDR. Plain http is only dialed to allowed CIDRs. The check runs on the addresses actually dialed, so there is no
// resolve-then-connect gap. Redirects are not followed and the response body is discarded.
//
// Bodies are signed like the budget and invoice webhooks: X-Gryvia-Signature: sha256=<hex HMAC(secret,
// "<X-Gryvia-Timestamp>.<body>")>.
package jobhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Guard decides which addresses may be dialed.
type Guard struct {
	// Allowed CIDRs may be dialed even when private, and over plain http.
	Allowed []*net.IPNet
	// Lookup resolves a host (tests); nil = net.DefaultResolver.
	Lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
	// Now is the clock (tests); nil = time.Now.
	Now func() time.Time
}

// ParseCIDRs parses a comma-separated list; single addresses become /32 or /128.
func ParseCIDRs(list string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			ip := net.ParseIP(s)
			if ip == nil {
				return nil, fmt.Errorf("invalid address %q", s)
			}
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			s = fmt.Sprintf("%s/%d", ip, bits)
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", s)
		}
		out = append(out, n)
	}
	return out, nil
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func (g Guard) allowed(ip net.IP) bool {
	for _, n := range g.Allowed {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Blocked reports whether ip must not be dialed.
func (g Guard) Blocked(ip net.IP) bool {
	if g.allowed(ip) {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip) {
		return true
	}
	if v4 := ip.To4(); v4 != nil && (v4[0] == 0 || v4[0] >= 240) {
		return true
	}
	return false
}

var headerName = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// reserved headers are set by the delivery and cannot be overridden.
var reserved = map[string]bool{"host": true, "content-type": true, "content-length": true, "user-agent": true,
	"connection": true, "transfer-encoding": true}

// CheckURL validates a hook URL before any dial.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("webhook URL must be an absolute http(s) URL")
	}
	if u.User != nil {
		return errors.New("webhook URL must not embed credentials")
	}
	if u.Fragment != "" {
		return errors.New("webhook URL must not have a fragment")
	}
	return nil
}

// CheckHeaders validates extra headers.
func CheckHeaders(h map[string]string) error {
	for k, v := range h {
		lk := strings.ToLower(k)
		if !headerName.MatchString(k) || reserved[lk] || strings.HasPrefix(lk, "x-gryvia-") {
			return fmt.Errorf("header %q is not allowed", k)
		}
		if len(v) > 1024 || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("header %q has an invalid value", k)
		}
	}
	return nil
}

// Sign returns the X-Gryvia-Signature value.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Request is one delivery.
type Request struct {
	URL      string
	Body     []byte
	Headers  map[string]string
	Secret   string
	Event    string
	Delivery string
	Timeout  time.Duration
}

func (g Guard) dialer(plainHTTP bool, timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	lookup := g.Lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var ips []net.IPAddr
		if ip := net.ParseIP(host); ip != nil {
			ips = []net.IPAddr{{IP: ip}}
		} else if ips, err = lookup(ctx, host); err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("webhook host %s has no address", host)
		}
		for _, a := range ips {
			if g.Blocked(a.IP) {
				return nil, fmt.Errorf("webhook host resolves to a blocked address (%s)", a.IP)
			}
			if plainHTTP && !g.allowed(a.IP) {
				return nil, fmt.Errorf("plain http is only allowed to the operator's allowed CIDRs (%s is not)", a.IP)
			}
		}
		var last error
		for _, a := range ips {
			c, err := d.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
}

// Post sends the request and returns the HTTP status; err is nil only for a 2xx answer.
func (g Guard) Post(ctx context.Context, r Request) (int, error) {
	if err := CheckURL(r.URL); err != nil {
		return 0, err
	}
	timeout := r.Timeout
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 10 * time.Second
	}
	u, _ := url.Parse(r.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		return 0, err
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	ts := strconv.FormatInt(now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "gryvia-job-hook")
	req.Header.Set("X-Gryvia-Timestamp", ts)
	req.Header.Set("X-Gryvia-Event", r.Event)
	req.Header.Set("X-Gryvia-Delivery", r.Delivery)
	if r.Secret != "" {
		req.Header.Set("X-Gryvia-Signature", Sign(r.Secret, ts, r.Body))
	}
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{DialContext: g.dialer(u.Scheme == "http", timeout), Proxy: nil,
			DisableKeepAlives: true, TLSHandshakeTimeout: timeout},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}
