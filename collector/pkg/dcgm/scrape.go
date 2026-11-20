// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package dcgm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultURL is dcgm-exporter's default listen address.
const DefaultURL = "http://127.0.0.1:9400/metrics"

// maxBody caps one scrape (a full exporter page for 8 GPUs is well below this).
const maxBody = 8 << 20

// Scraper polls dcgm-exporter into a Store. Every error is returned to the
// caller (the collector logs and carries on); a broken exporter never affects
// anything else.
type Scraper struct {
	URL    string
	Client *http.Client
	Store  *Store
	Now    func() time.Time
}

// ValidateURL accepts http/https URLs with a host only.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("dcgm url must be http(s)://host[:port]/path")
	}
	return nil
}

// ScrapeOnce fetches, parses and stores one scrape; it returns the number of GPU samples stored.
func (s *Scraper) ScrapeOnce(ctx context.Context) (int, error) {
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("dcgm-exporter answered %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return 0, err
	}
	if len(b) > maxBody {
		return 0, errors.New("dcgm-exporter response too large")
	}
	at := now()
	samples := Samples(at, Parse(string(b)))
	s.Store.Add(at, samples)
	return len(samples), nil
}

// Run scrapes every interval until ctx is done. onError may be nil.
func (s *Scraper) Run(ctx context.Context, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.ScrapeOnce(ctx); err != nil && onError != nil && ctx.Err() == nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
