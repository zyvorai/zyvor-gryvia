// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// gryvia-flight fetches a job's Flight Recorder report from the Gryvia API gateway.
//
// It talks to the gateway (GET /api/flight/jobs/{job}), not to collectors: the gateway holds the
// collector token, signs the per-node requests, applies tenant scoping and reports coverage. The
// credential used here is the gateway's API key or a session/OIDC token, never the collector token.
//
// Exit codes: 0 report fetched; 1 request or response failure; 2 usage error; 3 the report is
// partial or truncated and --require-complete was given.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxReport    = 4 << 20
	maxErrorBody = 4 << 10
	tokenEnv     = "GRYVIA_API_TOKEN"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// report holds the fields of the gateway response this tool interprets. Everything else is passed
// through untouched in JSON output.
type report struct {
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	Scope     string `json:"scope"`
	Coverage  struct {
		Total     int  `json:"total"`
		Reachable int  `json:"reachable"`
		Reporting int  `json:"reporting"`
		Complete  bool `json:"complete"`
	} `json:"coverage"`
	Truncated bool           `json:"truncated"`
	Nodes     []string       `json:"nodes"`
	Counts    map[string]int `json:"counts"`
	Findings  []struct {
		Node     string `json:"node"`
		Code     string `json:"code"`
		Evidence string `json:"evidence"`
	} `json:"findings"`
	Events []json.RawMessage `json:"events"`
}

// partial reports whether the view must not be read as complete.
func (r *report) partial() bool { return !r.Coverage.Complete || r.Truncated }

// statusError turns a non-200 gateway answer into an actionable message.
func statusError(code int, detail string) error {
	hint := ""
	switch code {
	case http.StatusUnauthorized:
		hint = " (the token was rejected)"
	case http.StatusForbidden:
		hint = " (this token may not read that namespace)"
	case http.StatusNotFound:
		hint = " (gateway route not found; is the gateway up to date?)"
	case http.StatusServiceUnavailable:
		hint = " (Flight Recorder token unset, or no collector reachable)"
	}
	if detail != "" {
		return fmt.Errorf("gateway returned HTTP %d%s: %s", code, hint, detail)
	}
	return fmt.Errorf("gateway returned HTTP %d%s", code, hint)
}

// fetch returns the raw report body after validating it is a JSON object.
func fetch(ctx context.Context, client *http.Client, gateway, namespace, job, token string) ([]byte, error) {
	return fetchPath(ctx, client, gateway, "/api/flight/jobs/", namespace, job, token, "")
}

// fetchPath is fetch for any gateway route of the form <prefix><job><suffix>?namespace=<namespace>, for example
// "/api/flight/jobs/" + job + "/diagnosis" or "/api/flight/inference/" + job.
func fetchPath(ctx context.Context, client *http.Client, gateway, prefix, namespace, job, token, suffix string) ([]byte, error) {
	u, err := url.Parse(gateway)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("gateway must be an HTTP(S) URL without credentials, query or fragment")
	}
	// The bearer token may only cross plaintext on an explicit localhost tunnel.
	if u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return nil, errors.New("use HTTPS or a localhost port-forward for the gateway")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("empty token")
	}
	if len(job) > 63 || len(namespace) > 63 || !dnsLabel.MatchString(job) || !dnsLabel.MatchString(namespace) {
		return nil, errors.New("namespace and job must be DNS-1123 labels")
	}
	u.Path = strings.TrimRight(u.Path, "/") + prefix + job + suffix
	u.RawPath = ""
	q := url.Values{}
	q.Set("namespace", namespace)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	// Never forward the credential to a redirect target (for example a login page).
	safe := *client
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := safe.Do(req)
	if err != nil {
		// url.Error text contains the URL only, never headers.
		return nil, fmt.Errorf("gateway request failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
		var e struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(raw, &e) // the gateway's detail is a plain sentence; anything else is dropped
		return nil, statusError(res.StatusCode, strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return ' '
			}
			return r
		}, e.Detail))
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxReport+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxReport {
		return nil, errors.New("gateway report exceeds 4 MiB")
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(body, &probe) != nil {
		return nil, errors.New("gateway returned an invalid report")
	}
	return body, nil
}

// printText writes a short human summary. Untrusted strings are printed with %q so control
// characters in event or finding text cannot alter the terminal.
func printText(out io.Writer, r *report) {
	fmt.Fprintf(out, "Job %s/%s\n", r.Namespace, r.Job)
	c := r.Coverage
	fmt.Fprintf(out, "Collectors: %d reachable of %d discovered, %d returned events for this job\n", c.Reachable, c.Total, c.Reporting)
	if r.partial() {
		why := []string{}
		if !c.Complete {
			why = append(why, "not every discovered collector answered")
		}
		if r.Truncated {
			why = append(why, "events were dropped by a limit")
		}
		fmt.Fprintf(out, "PARTIAL VIEW: %s. Missing events do not establish healthy behavior.\n", strings.Join(why, "; "))
	} else {
		fmt.Fprintln(out, "Every discovered collector answered. Nodes without a running collector are not counted.")
	}
	kinds := make([]string, 0, len(r.Counts))
	for k := range r.Counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	fmt.Fprintf(out, "Events: %d returned\n", len(r.Events))
	for _, k := range kinds {
		fmt.Fprintf(out, "  %-24q %d\n", k, r.Counts[k])
	}
	if len(r.Findings) == 0 {
		fmt.Fprintln(out, "Findings: none in the retained sample")
		return
	}
	fmt.Fprintf(out, "Findings: %d\n", len(r.Findings))
	for _, f := range r.Findings {
		fmt.Fprintf(out, "  %q %q: %q\n", f.Node, f.Code, f.Evidence)
	}
}

// diagnosisReport holds the fields of the gateway diagnosis this tool interprets.
type diagnosisReport struct {
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	Summary   string `json:"summary"`
	Partial   bool   `json:"partial"`
	Findings  []struct {
		Kind               string   `json:"kind"`
		Severity           string   `json:"severity"`
		Confidence         string   `json:"confidence"`
		Nodes              []string `json:"nodes"`
		Summary            string   `json:"summary"`
		WhatWasNotMeasured []string `json:"whatWasNotMeasured"`
		Evidence           []struct {
			Node   string  `json:"node"`
			Source string  `json:"source"`
			Metric string  `json:"metric"`
			Value  float64 `json:"value"`
			Window string  `json:"window"`
		} `json:"evidence"`
	} `json:"findings"`
	Unavailable []struct {
		Node   string `json:"node"`
		Signal string `json:"signal"`
		Reason string `json:"reason"`
	} `json:"unavailable"`
	Completeness struct {
		ProbesAttached int `json:"probesAttached"`
		ProbesSkipped  []struct {
			Node   string `json:"node"`
			Object string `json:"object"`
			Reason string `json:"reason"`
		} `json:"probesSkipped"`
		DroppedEvents struct {
			Total uint64 `json:"total"`
		} `json:"droppedEvents"`
		Sampling struct {
			Ratio float64 `json:"ratio"`
		} `json:"sampling"`
		// NodesExpected is a number, or the string "unknown".
		NodesExpected  json.RawMessage `json:"nodesExpected"`
		NodesReporting int             `json:"nodesReporting"`
		MissingNodes   []string        `json:"missingNodes"`
		Complete       bool            `json:"complete"`
		Reasons        []string        `json:"reasons"`
	} `json:"measurementCompleteness"`
}

// incomplete reports whether the diagnosis must not be read as covering the whole job.
func (d *diagnosisReport) incomplete() bool { return d.Partial || !d.Completeness.Complete }

// printDiagnosis writes a human summary. Untrusted strings are printed with %q.
func printDiagnosis(out io.Writer, d *diagnosisReport) {
	c := d.Completeness
	fmt.Fprintf(out, "Job %s/%s\n%q\n", d.Namespace, d.Job, d.Summary)
	if d.incomplete() {
		fmt.Fprintln(out, "INCOMPLETE MEASUREMENT: absence of findings is not evidence of health.")
	}
	fmt.Fprintf(out, "Nodes: %d reported of %s expected; missing: %q\n", c.NodesReporting, strings.Trim(string(c.NodesExpected), `"`), c.MissingNodes)
	fmt.Fprintf(out, "Probes attached: %d, skipped: %d; dropped events: %d; sampling ratio: %g\n", c.ProbesAttached, len(c.ProbesSkipped), c.DroppedEvents.Total, c.Sampling.Ratio)
	for i, f := range d.Findings {
		fmt.Fprintf(out, "%d. %q %s (%s confidence) on %q\n   %q\n", i+1, f.Kind, f.Severity, f.Confidence, f.Nodes, f.Summary)
		for _, e := range f.Evidence {
			fmt.Fprintf(out, "   evidence: %q %q %q = %g (window %q)\n", e.Node, e.Source, e.Metric, e.Value, e.Window)
		}
		for _, m := range f.WhatWasNotMeasured {
			fmt.Fprintf(out, "   not measured: %q\n", m)
		}
	}
	if len(d.Unavailable) > 0 {
		fmt.Fprintf(out, "Unavailable (not healthy): %d\n", len(d.Unavailable))
		for _, u := range d.Unavailable {
			fmt.Fprintf(out, "  %q %q: %q\n", u.Node, u.Signal, u.Reason)
		}
	}
	for _, r := range c.Reasons {
		fmt.Fprintf(out, "Incomplete because: %q\n", r)
	}
}

// inference holds the serving-engine figures the gateway reads from GryviaFabricSignal.status. A nil
// pointer means "not measured" (the engine does not export it, or no request finished in the window).
type inference struct {
	Available       bool     `json:"available"`
	Engine          string   `json:"engine"`
	UpdatedAt       string   `json:"updatedAt"`
	TTFTP99         *float64 `json:"ttftP99ms"`
	ITLP99          *float64 `json:"itlP99ms"`
	QueueP99        *float64 `json:"queueTimeP99ms"`
	E2EP99          *float64 `json:"e2eP99ms"`
	QueueMean       *float64 `json:"queueTimeMeanMs"`
	E2EMean         *float64 `json:"e2eMeanMs"`
	RequestsWaiting *float64 `json:"requestsWaiting"`
	KVCacheUsage    *float64 `json:"kvCacheUsage"`
	InferWaitP99    *float64 `json:"inferWaitP99ms"`
}

func ms(v *float64) string {
	switch {
	case v == nil || *v < 0:
		return "not measured"
	case *v < 1:
		return fmt.Sprintf("%.2f ms", *v)
	case *v < 1000:
		return strconv.FormatFloat(math.Round(*v*10)/10, 'f', -1, 64) + " ms"
	}
	return fmt.Sprintf("%.2f s", *v/1000)
}

// printInference writes the -inference section. Untrusted strings are printed with %q.
func printInference(out io.Writer, in *inference) {
	if !in.Available {
		fmt.Fprintln(out, "Inference: no engine metrics published for this job (the collector's -infer-metrics scraper is off, or no GryviaFabricSignal with this jobRef reports an engine)")
		return
	}
	fmt.Fprintf(out, "Inference: engine %q, updated %q. Reported by the engine itself, not measured by the network probes.\n", in.Engine, in.UpdatedAt)
	fmt.Fprintf(out, "  time to first token p99   %s\n", ms(in.TTFTP99))
	fmt.Fprintf(out, "  inter-token latency p99   %s\n", ms(in.ITLP99))
	fmt.Fprintf(out, "  engine queue time p99     %s\n", ms(in.QueueP99))
	fmt.Fprintf(out, "  end-to-end latency p99    %s\n", ms(in.E2EP99))
	if in.QueueP99 == nil && in.QueueMean != nil {
		fmt.Fprintf(out, "  engine queue time MEAN    %s (this engine exports only cumulative durations)\n", ms(in.QueueMean))
	}
	if in.E2EP99 == nil && in.E2EMean != nil {
		fmt.Fprintf(out, "  end-to-end latency MEAN   %s (this engine exports only cumulative durations)\n", ms(in.E2EMean))
	}
	if in.RequestsWaiting != nil {
		fmt.Fprintf(out, "  requests waiting          %.0f\n", *in.RequestsWaiting)
	} else {
		fmt.Fprintln(out, "  requests waiting          not measured")
	}
	if in.KVCacheUsage != nil {
		fmt.Fprintf(out, "  KV cache usage            %.0f%%\n", 100**in.KVCacheUsage)
	} else {
		fmt.Fprintln(out, "  KV cache usage            not measured")
	}
	fmt.Fprintf(out, "  network accept wait p99   %s (accept to first read on the socket; not queue time)\n", ms(in.InferWaitP99))
}

func readToken(file string, stdin io.Reader, getenv func(string) string) (string, error) {
	var raw []byte
	var err error
	switch {
	case file == "-":
		raw, err = io.ReadAll(io.LimitReader(stdin, 8193))
	case file != "":
		var f *os.File
		if f, err = os.Open(file); err == nil {
			raw, err = io.ReadAll(io.LimitReader(f, 8193))
			f.Close()
		}
	default:
		raw = []byte(getenv(tokenEnv))
	}
	if err != nil {
		return "", fmt.Errorf("cannot read token: %w", err)
	}
	if len(raw) > 8192 {
		return "", errors.New("token is too long")
	}
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return "", fmt.Errorf("no token: use --token-file (or - for stdin) or set %s", tokenEnv)
	}
	return t, nil
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string, client *http.Client) int {
	fs := flag.NewFlagSet("gryvia-flight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: gryvia-flight -namespace NS -job JOB [-token-file FILE|-] [flags]\n\n"+
			"Fetches the Flight Recorder report from the Gryvia API gateway. The token is the gateway\n"+
			"API key or a session/OIDC token (not the collector token); it is read from a file, stdin\n"+
			"or $%s, never from a flag value.\n\nFlags:\n", tokenEnv)
		fs.PrintDefaults()
	}
	gateway := fs.String("gateway", "https://localhost:8080", "API gateway URL (HTTPS, or http on localhost via port-forward)")
	namespace := fs.String("namespace", "", "job namespace (required)")
	job := fs.String("job", "", "job name (required)")
	tokenFile := fs.String("token-file", "", "file holding the gateway token, or - for stdin (default: $"+tokenEnv+")")
	output := fs.String("o", "json", "output format: json or text")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout (the gateway's fan-out may take up to 10s)")
	requireComplete := fs.Bool("require-complete", false, "exit 3 when the report is partial or truncated (with -diagnosis: when the measurement is incomplete)")
	diag := fs.Bool("diagnosis", false, "fetch the unified bottleneck diagnosis (ranked findings with evidence, unavailable telemetry and measurement completeness) instead of the event report")
	withInference := fs.Bool("inference", false, "also fetch the serving engine's own latency (TTFT, ITL, queue time; needs the collector's -infer-metrics). With -o json the output becomes {\"flight\": ..., \"inference\": ...}")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usage := func(msg string) int { fmt.Fprintln(stderr, "gryvia-flight:", msg); return 2 }
	if fs.NArg() != 0 {
		return usage("unexpected arguments")
	}
	if *namespace == "" || *job == "" {
		return usage("-namespace and -job are required")
	}
	if *output != "json" && *output != "text" {
		return usage("-o must be json or text")
	}
	if *timeout <= 0 {
		return usage("-timeout must be positive")
	}
	if *diag && *withInference {
		return usage("-diagnosis and -inference cannot be combined")
	}
	token, err := readToken(*tokenFile, stdin, getenv)
	if err != nil {
		return usage(err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if *diag {
		body, err := fetchPath(ctx, client, *gateway, "/api/flight/jobs/", *namespace, *job, token, "/diagnosis")
		if err != nil {
			fmt.Fprintln(stderr, "gryvia-flight:", err)
			return 1
		}
		var d diagnosisReport
		if err := json.Unmarshal(body, &d); err != nil {
			fmt.Fprintln(stderr, "gryvia-flight: gateway returned an unexpected diagnosis shape")
			return 1
		}
		if *output == "text" {
			printDiagnosis(stdout, &d)
		} else {
			fmt.Fprintln(stdout, strings.TrimRight(string(body), "\r\n"))
			if d.incomplete() {
				fmt.Fprintln(stderr, "gryvia-flight: warning: incomplete measurement (see measurementCompleteness); no finding is not evidence of health")
			}
		}
		if d.incomplete() && *requireComplete {
			return 3
		}
		return 0
	}
	body, err := fetch(ctx, client, *gateway, *namespace, *job, token)
	if err != nil {
		fmt.Fprintln(stderr, "gryvia-flight:", err)
		return 1
	}
	var r report
	if err := json.Unmarshal(body, &r); err != nil {
		fmt.Fprintln(stderr, "gryvia-flight: gateway returned an unexpected report shape")
		return 1
	}
	var inferenceBody []byte
	var inf inference
	if *withInference {
		inferenceBody, err = fetchPath(ctx, client, *gateway, "/api/flight/inference/", *namespace, *job, token, "")
		if err == nil && json.Unmarshal(inferenceBody, &inf) != nil {
			err = errors.New("gateway returned an unexpected inference shape")
		}
		if err != nil {
			fmt.Fprintln(stderr, "gryvia-flight: inference:", err)
			return 1
		}
	}
	if *output == "text" {
		printText(stdout, &r)
		if *withInference {
			printInference(stdout, &inf)
		}
	} else {
		flightJSON := strings.TrimRight(string(body), "\r\n")
		if *withInference {
			fmt.Fprintf(stdout, "{\"flight\":%s,\"inference\":%s}\n", flightJSON, strings.TrimRight(string(inferenceBody), "\r\n"))
		} else {
			fmt.Fprintln(stdout, flightJSON)
		}
		if r.partial() {
			fmt.Fprintln(stderr, "gryvia-flight: warning: partial or truncated view (see coverage and truncated)")
		}
	}
	if r.partial() && *requireComplete {
		return 3
	}
	return 0
}

func main() {
	// Default transport: TLS verification on, system roots, proxy from the environment.
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv, &http.Client{Timeout: 60 * time.Second}))
}
