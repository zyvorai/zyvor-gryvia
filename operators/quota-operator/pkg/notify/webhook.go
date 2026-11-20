// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package notify delivers budget alerts to one operator-configured webhook URL.
//
// It is a delivery seam only: nothing is paid, emailed or paged by Gryvia. The signature scheme matches the
// gateway's invoice webhook (services/api-gateway/routers/invoice_webhook.py) so a receiver can verify both
// the same way: header X-Gryvia-Signature: sha256=<hex HMAC(secret, "<timestamp>.<body>")> with the unix
// seconds in X-Gryvia-Timestamp. Receivers should reject old timestamps. Without a secret the body is unsigned.
//
// SSRF hardening: https is required unless the host is loopback; credentials in the URL are refused; the
// connection is refused when the address it is about to dial is link-local (including the 169.254.169.254
// metadata address), multicast, unspecified or the IPv4 broadcast/reserved class. The check runs in the dialer
// on the address actually used, so there is no resolve-then-connect gap. Private (RFC 1918) addresses are
// allowed, because in-cluster receivers have them. Redirects are never followed; the timeout is bounded.
package notify

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
	"strconv"
	"time"
)

// Timeout bounds one delivery.
const Timeout = 10 * time.Second

// Webhook posts JSON to URL.
type Webhook struct {
	URL    string
	Secret string
	// Now is the clock (tests); nil = time.Now.
	Now func() time.Time
	// dial overrides the dialer (tests); nil = a net.Dialer guarded by blockedAddr.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Sign returns the X-Gryvia-Signature value.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// CheckURL returns an error when the URL must not be called.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("webhook URL must be an absolute http(s) URL")
	}
	if u.User != nil {
		return errors.New("webhook URL must not embed credentials")
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return errors.New("webhook URL must use https")
	}
	return nil
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// blockedAddr reports whether dialing ip must be refused. loopbackOK lets a loopback URL reach loopback.
func blockedAddr(ip net.IP, loopbackOK bool) bool {
	switch {
	case ip.IsLoopback():
		return !loopbackOK
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast(), ip.IsUnspecified():
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] >= 240 { // reserved class E and the broadcast address
		return true
	}
	return false
}

func (w *Webhook) client(loopbackOK bool) *http.Client {
	dial := w.dial
	if dial == nil {
		d := &net.Dialer{Timeout: Timeout}
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, a := range ips {
				if blockedAddr(a.IP, loopbackOK) {
					return nil, fmt.Errorf("webhook host resolves to a blocked address (%s)", a.IP)
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
	return &http.Client{
		Timeout:       Timeout,
		Transport:     &http.Transport{DialContext: dial, Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Post sends body (JSON) and returns nil only for a 2xx response.
func (w *Webhook) Post(ctx context.Context, body []byte) error {
	if err := CheckURL(w.URL); err != nil {
		return err
	}
	u, _ := url.Parse(w.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	ts := strconv.FormatInt(now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gryvia-Timestamp", ts)
	if w.Secret != "" {
		req.Header.Set("X-Gryvia-Signature", Sign(w.Secret, ts, body))
	}
	resp, err := w.client(isLoopbackHost(u.Hostname())).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
