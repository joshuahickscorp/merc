package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var (
	cliVersion   = "dev"
	cliCommit    = "unknown"
	cliBuildDate = "unknown"
)

type jobType struct {
	Type        string  `json:"type"`
	BatchSize   int     `json:"batch_size,omitempty"`
	MaxTokens   uint32  `json:"max_tokens,omitempty"`
	Temperature float32 `json:"temperature,omitempty"`
}

type modelRef struct {
	Kind string `json:"kind,omitempty"`
	Ref  string `json:"ref"`
}

type jobConstraints struct {
	MinMemoryGB   float32  `json:"min_memory_gb"`
	HWClasses     []string `json:"hw_classes,omitempty"`
	DataResidency []string `json:"data_residency,omitempty"`
}

type verificationPolicy struct {
	RedundancyFrac float32 `json:"redundancy_frac"`
	HoneypotFrac   float32 `json:"honeypot_frac"`
	PayoutHoldSecs uint32  `json:"payout_hold_secs"`
}

type cliJobSubmit struct {
	JobType      jobType            `json:"job_type"`
	Model        modelRef           `json:"model"`
	Params       json.RawMessage    `json:"params,omitempty"`
	Constraints  jobConstraints     `json:"constraints"`
	Verification verificationPolicy `json:"verification"`
	Tier         string             `json:"tier"`
	Input        json.RawMessage    `json:"input"`
	WebhookURL   string             `json:"webhook_url,omitempty"`
	MaxUSD       float64            `json:"max_usd,omitempty"`
	QuoteID      string             `json:"quote_id,omitempty"`
}

type client struct {
	base string
	key  string
	hc   *http.Client
}

func newClient() *client {
	base := strings.TrimRight(envOr("CX_API_URL", "http://localhost:8080"), "/")
	return &client{base: base, key: os.Getenv("CX_API_KEY"), hc: &http.Client{Timeout: 60 * time.Second}}
}

func (c *client) do(method, path string, body []byte) []byte {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		fatalf("building request: %v", err)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fatalf("%s %s -> %s\n%s", method, path, resp.Status, strings.TrimSpace(string(out)))
	}
	return out
}

func dispatchBuyer(cmd string, args []string) bool {
	switch cmd {
	case "submit":
		cmdSubmit(args)
	case "quote":
		cmdQuote(args)
	case "status":
		cmdStatus(args)
	case "results":
		cmdResults(args)
	case "invoice":
		cmdInvoice(args)
	case "events":
		cmdEvents(args)
	case "failures":
		cmdFailures(args)
	case "models":
		cmdModels(args)
	case "estimate":
		cmdEstimate(args)
	case "explain-scheduler":
		cmdExplainScheduler(args)
	case "cancel":
		cmdCancel(args)
	case "audit":
		cmdAudit(args)
	case "source-id":
		cmdSourceID(args)
	case "verify":
		cmdVerify(args)
	case "prove":
		cmdProve(args)
	case "version":
		cmdVersion(args)
	case "-h", "--help", "help":
		usage()
	default:
		return false // not a buyer command: let main() fall through to serve
	}
	return true
}

type cliVersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

func cmdVersion(args []string) {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print machine-readable release identity")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fatalf("version accepts no positional arguments")
	}
	info := cliVersionInfo{
		Version: cliVersion, Commit: cliCommit, BuildDate: cliBuildDate,
		GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if *asJSON {
		out, err := json.Marshal(info)
		if err != nil {
			fatalf("encoding version: %v", err)
		}
		fmt.Println(string(out))
		return
	}
	fmt.Printf("cx %s (%s, %s, %s, %s)\n", info.Version, info.Commit, info.BuildDate, info.GoVersion, info.Platform)
}

func cmdSubmit(args []string) {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	model := fs.String("model", "", "model id, e.g. all-minilm-l6-v2 (required)")
	typ := fs.String("type", "", "job type: embed|batch_infer (required)")
	input := fs.String("input", "-", "JSONL input file, or - for stdin")
	tier := fs.String("tier", "batch", "service tier: batch|priority|trusted")
	maxTokens := fs.Uint("max-tokens", 0, "max tokens (batch_infer)")
	temperature := fs.Float64("temperature", 0, "sampling temperature (batch_infer)")
	batchSize := fs.Uint("batch-size", 0, "embedding batch size (embed)")
	redundancy := fs.Float64("redundancy", 0, "redundancy fraction 0.0-1.0")
	honeypot := fs.Float64("honeypot", 0, "honeypot fraction 0.0-1.0")
	payoutHold := fs.Uint("payout-hold", 0, "payout hold seconds")
	split := fs.Int("split", 0, "lines per task (0 = server adaptive default)")
	minMemory := fs.Float64("min-memory", 0, "min worker memory GB")
	hwClasses := fs.String("hw-classes", "", "comma-separated allowed hw classes")
	dataResidency := fs.String("data-residency", "", "comma-separated allowed country codes")
	webhook := fs.String("webhook", "", "https completion webhook URL")
	quoteID := fs.String("quote-id", "", "bind to an advisory quote id (q_<uuid> from `cx quote`)")
	maxUSD := fs.Float64("max-usd", 0, "hard spend cap in USD (Budget Governor); 0 = no cap")
	s3Key := fs.String("s3-key", "", "use an already-uploaded object instead of --input")
	wait := fs.Bool("wait", false, "poll to completion and print results")
	poll := fs.Duration("poll", 3*time.Second, "poll interval with --wait")
	timeout := fs.Duration("timeout", 30*time.Minute, "give up waiting after this")
	fs.Parse(args)

	if *model == "" || *typ == "" {
		fatalf("--model and --type are required")
	}
	if !validJobTypes[*typ] {
		fatalf("--type must be embed or batch_infer")
	}

	jt := jobType{Type: *typ}
	if *batchSize > 0 {
		jt.BatchSize = int(*batchSize)
	}
	if *maxTokens > 0 {
		jt.MaxTokens = uint32(*maxTokens)
	}
	if *temperature > 0 {
		jt.Temperature = float32(*temperature)
	}

	var inputField json.RawMessage
	if *s3Key != "" {
		inputField = mustJSON(map[string]string{"s3_key": *s3Key})
	} else {
		data := readInput(*input)
		if len(bytes.TrimSpace(data)) == 0 {
			fatalf("input is empty (pass --input <file> or pipe JSONL on stdin)")
		}
		inputField = mustJSON(string(data)) // a JSON string IS the inline JSONL
	}

	var params json.RawMessage
	if *split > 0 {
		params = mustJSON(map[string]int{"split_size": *split})
	}

	sub := cliJobSubmit{
		JobType: jt,
		Model:   modelRef{Ref: *model},
		Params:  params,
		Constraints: jobConstraints{
			MinMemoryGB:   float32(*minMemory),
			HWClasses:     splitCSV(*hwClasses),
			DataResidency: splitCSV(*dataResidency),
		},
		Verification: verificationPolicy{
			RedundancyFrac: float32(*redundancy),
			HoneypotFrac:   float32(*honeypot),
			PayoutHoldSecs: uint32(*payoutHold),
		},
		Tier:       *tier,
		Input:      inputField,
		WebhookURL: *webhook,
		MaxUSD:     *maxUSD,
		QuoteID:    *quoteID,
	}

	c := newClient()
	out := c.do("POST", "/v1/jobs", mustJSON(sub))
	printJSON(out)

	var sr struct {
		JobID string `json:"job_id"`
	}
	json.Unmarshal(out, &sr)
	if sr.JobID == "" {
		fatalf("server response did not include a job_id")
	}
	if *wait {
		waitForJob(c, sr.JobID, *poll, *timeout)
	}
}

func waitForJob(c *client, id string, poll, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		var js struct {
			Status    string `json:"status"`
			TasksDone int    `json:"tasks_done"`
			TaskCount int    `json:"task_count"`
		}
		json.Unmarshal(c.do("GET", "/v1/jobs/"+id, nil), &js)
		fmt.Fprintf(os.Stderr, "status=%s tasks=%d/%d\n", js.Status, js.TasksDone, js.TaskCount)
		switch js.Status {
		case "complete":
			fetchResults(c, id)
			return
		case "failed", "cancelled":
			fatalf("job %s ended with status %q", id, js.Status)
		}
		if time.Now().After(deadline) {
			fatalf("timed out after %s waiting for job %s (last status %q)", timeout, id, js.Status)
		}
		time.Sleep(poll)
	}
}

func cmdStatus(args []string) {
	id := oneArg("status", args)
	out := newClient().do("GET", "/v1/jobs/"+id, nil)
	var js statusResp
	if json.Unmarshal(out, &js) == nil {
		if js.ChargeStatus != "" {
			fmt.Fprintf(os.Stderr, "charge_status=%s\n", js.ChargeStatus)
		}
		v := js.Verification
		fmt.Fprintf(os.Stderr,
			"verification=%s checked=%d honeypots=%d/%d redundancy=%d/%d tiebreaks=%d dispute=%q\n",
			v.Label, v.Checked, v.HoneypotsPassed, v.HoneypotsFailed,
			v.RedundancyMatched, v.RedundancyMismatched, v.Tiebreaks, v.DisputeStatus)
	}
	printJSON(out)
}

type statusResp struct {
	ChargeStatus string `json:"charge_status"`
	Verification struct {
		Checked              int    `json:"checked"`
		HoneypotsPassed      int    `json:"honeypots_passed"`
		HoneypotsFailed      int    `json:"honeypots_failed"`
		RedundancyMatched    int    `json:"redundancy_matched"`
		RedundancyMismatched int    `json:"redundancy_mismatched"`
		Tiebreaks            int    `json:"tiebreaks"`
		DisputeStatus        string `json:"dispute_status"`
		Label                string `json:"label"`
	} `json:"verification"`
}

func cmdCancel(args []string) {
	id := oneArg("cancel", args)
	printJSON(newClient().do("DELETE", "/v1/jobs/"+id, nil))
}

func cmdResults(args []string) {
	id := oneArg("results", args)
	fetchResults(newClient(), id)
}

func cmdEvents(args []string) {
	id := oneArg("events", args)
	printJSON(newClient().do("GET", "/v1/jobs/"+id+"/events", nil))
}

func cmdFailures(args []string) {
	id := oneArg("failures", args)
	printJSON(newClient().do("GET", "/v1/jobs/"+id+"/failures", nil))
}

type invoiceResp struct {
	JobID           string   `json:"job_id"`
	Status          string   `json:"status"`
	JobType         string   `json:"job_type"`
	EstimatedUSD    float64  `json:"estimated_usd"`
	ActualUSD       float64  `json:"actual_usd"`
	ChargedUSD      float64  `json:"charged_usd"`
	SupplierPaidUSD float64  `json:"supplier_credit_usd"`
	PlatformTakeUSD float64  `json:"platform_take_usd"`
	QuotedUSD       *float64 `json:"quoted_usd,omitempty"`
}

func cmdInvoice(args []string) {
	fs := flag.NewFlagSet("invoice", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the full invoice JSON")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
		fatalf("usage: cx invoice [--json] <job_id>")
	}
	id := rest[0]
	out := newClient().do("GET", "/v1/jobs/"+id+"/invoice", nil)
	if *asJSON {
		printJSON(out)
		return
	}
	var inv invoiceResp
	if err := json.Unmarshal(out, &inv); err != nil {
		printJSON(out) // fall back to raw on any shape drift
		return
	}
	printInvoice(inv)
}

func printInvoice(inv invoiceResp) {
	p := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }
	p("Invoice %s", inv.JobID)
	p("  Workload : %s (%s)", inv.JobType, inv.Status)
	p("  Estimated: $%.4f", inv.EstimatedUSD)
	p("  Actual   : $%.4f", inv.ActualUSD)
	p("  Charged  : $%.4f", inv.ChargedUSD)
	if inv.QuotedUSD != nil {
		p("  Quoted   : $%.4f (delta $%+.4f vs charged)", *inv.QuotedUSD, inv.ChargedUSD-*inv.QuotedUSD)
	}
	p("  Supplier : $%.4f credit", inv.SupplierPaidUSD)
	p("  Platform : $%.4f take", inv.PlatformTakeUSD)
}

func fetchResults(c *client, id string) {
	var jr struct {
		Status     string   `json:"status"`
		ResultsURL string   `json:"results_url"`
		ResultURLs []string `json:"result_urls"`
	}
	json.Unmarshal(c.do("GET", "/v1/jobs/"+id+"/results", nil), &jr)
	if jr.ResultsURL != "" {
		streamURL(c, jr.ResultsURL)
		return
	}
	if len(jr.ResultURLs) == 0 {
		fatalf("job %s has no results yet (status %q)", id, jr.Status)
	}
	fmt.Fprintf(os.Stderr, "no merged artifact; streaming %d per-task results\n", len(jr.ResultURLs))
	for _, u := range jr.ResultURLs {
		streamURL(c, u)
	}
}

func streamURL(c *client, u string) {
	resp, err := c.hc.Get(u)
	if err != nil {
		fatalf("downloading result: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fatalf("downloading result -> %s\n%s", resp.Status, strings.TrimSpace(string(body)))
	}
	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		fatalf("streaming result body: %v", err)
	}
}

func cmdModels(args []string) {
	printJSON(newClient().do("GET", "/v1/models", nil))
}

func cmdEstimate(args []string) {
	fs := flag.NewFlagSet("estimate", flag.ExitOnError)
	model := fs.String("model", "", "model id (required)")
	units := fs.Uint64("units", 0, "unit count, e.g. tokens/embeddings (required)")
	tier := fs.String("tier", "batch", "service tier")
	fs.Parse(args)
	if *model == "" || *units == 0 {
		fatalf("--model and a positive --units are required")
	}
	q := url.Values{}
	q.Set("model", *model)
	q.Set("units", strconv.FormatUint(*units, 10))
	q.Set("tier", *tier)
	printJSON(newClient().do("GET", "/v1/price-estimate?"+q.Encode(), nil))
}

func cmdExplainScheduler(args []string) {
	fs := flag.NewFlagSet("explain-scheduler", flag.ExitOnError)
	worker := fs.String("worker", "", "worker id (uuid) to explain (required)")
	fs.Parse(args)
	if *worker == "" {
		fatalf("--worker <id> is required")
	}
	q := url.Values{}
	q.Set("worker_id", *worker)
	printJSON(newClient().do("GET", "/admin/scheduler/explain?"+q.Encode(), nil))
}

type quoteResp struct {
	QuoteID       string `json:"quote_id"`
	JobType       string `json:"job_type"`
	Model         string `json:"model"`
	Tier          string `json:"tier"`
	TierSemantics string `json:"tier_semantics"`
	Input         struct {
		Records          int   `json:"records"`
		Bytes            int   `json:"bytes"`
		EstimatedTokens  int64 `json:"estimated_tokens"`
		MalformedRecords int   `json:"malformed_records"`
		FirstBadLine     int   `json:"first_bad_line"`
	} `json:"input"`
	Execution struct {
		RecommendedSplitSize int    `json:"recommended_split_size"`
		EstimatedTasks       int    `json:"estimated_tasks"`
		EligibleWorkersNow   int    `json:"eligible_workers_now"`
		OOMRisk              string `json:"oom_risk"`
		ColdStartRisk        string `json:"cold_start_risk"`
	} `json:"execution"`
	Cost struct {
		MinUSD      float64 `json:"min_usd"`
		ExpectedUSD float64 `json:"expected_usd"`
		MaxUSD      float64 `json:"max_usd"`
	} `json:"cost"`
	Time struct {
		P50Secs int `json:"p50_secs"`
		P90Secs int `json:"p90_secs"`
	} `json:"time"`
	Budget struct {
		SuggestedMaxUSD float64 `json:"suggested_max_usd"`
	} `json:"budget"`
	Warnings []string `json:"warnings"`
}

func cmdQuote(args []string) {
	fs := flag.NewFlagSet("quote", flag.ExitOnError)
	model := fs.String("model", "", "model id, e.g. all-minilm-l6-v2 (required)")
	typ := fs.String("type", "", "job type: embed|batch_infer (required)")
	input := fs.String("input", "-", "JSONL input file, or - for stdin")
	tier := fs.String("tier", "batch", "service tier: batch|priority|trusted")
	split := fs.Int("split", 0, "lines per task (0 = server adaptive default)")
	minMemory := fs.Float64("min-memory", 0, "min worker memory GB")
	redundancy := fs.Float64("redundancy", 0, "redundancy fraction 0.0-1.0")
	asJSON := fs.Bool("json", false, "print the full quote JSON")
	fs.Parse(args)
	if *model == "" || *typ == "" {
		fatalf("--model and --type are required")
	}
	if !validJobTypes[*typ] {
		fatalf("--type must be embed or batch_infer")
	}
	data := readInput(*input)
	if len(bytes.TrimSpace(data)) == 0 {
		fatalf("input is empty (pass --input <file> or pipe JSONL on stdin)")
	}
	var params json.RawMessage
	if *split > 0 {
		params = mustJSON(map[string]int{"split_size": *split})
	}
	sub := cliJobSubmit{
		JobType:      jobType{Type: *typ},
		Model:        modelRef{Ref: *model},
		Params:       params,
		Constraints:  jobConstraints{MinMemoryGB: float32(*minMemory)},
		Verification: verificationPolicy{RedundancyFrac: float32(*redundancy)},
		Tier:         *tier,
		Input:        mustJSON(string(data)),
	}
	out := newClient().do("POST", "/v1/quote", mustJSON(sub))
	if *asJSON {
		printJSON(out)
		return
	}
	var q quoteResp
	if err := json.Unmarshal(out, &q); err != nil {
		printJSON(out) // fall back to raw on any shape drift
		return
	}
	printQuote(q, *model, *typ, *tier, *input)
}

func printQuote(q quoteResp, model, typ, tier, inputPath string) {
	p := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }
	p("Quote %s", q.QuoteID)
	p("  Workload : %s, %s", q.JobType, q.Model)
	p("  Input    : %d records, ~%s tokens, %s", q.Input.Records, human(q.Input.EstimatedTokens), humanBytes(q.Input.Bytes))
	if q.Input.MalformedRecords > 0 {
		p("  ⚠ Input  : %d malformed record(s); first at line %d", q.Input.MalformedRecords, q.Input.FirstBadLine)
	}
	p("  Plan     : %d tasks, split_size=%d, %s tier", q.Execution.EstimatedTasks, q.Execution.RecommendedSplitSize, q.Tier)
	if q.TierSemantics != "" {
		p("  Service  : %s", q.TierSemantics)
	}
	p("  Supply   : %d eligible now", q.Execution.EligibleWorkersNow)
	p("  Cost     : $%.4f-$%.4f expected $%.4f", q.Cost.MinUSD, q.Cost.MaxUSD, q.Cost.ExpectedUSD)
	p("  ETA      : p50 %s, p90 %s", humanSecs(q.Time.P50Secs), humanSecs(q.Time.P90Secs))
	p("  Risk     : %s OOM, %s cold-start", q.Execution.OOMRisk, q.Execution.ColdStartRisk)
	for _, w := range q.Warnings {
		p("  ⚠ %s", w)
	}
	p("  Cap      : --max-usd %.4f (suggested)", q.Budget.SuggestedMaxUSD)
	p("  Submit   : cx submit --model %s --type %s --tier %s --input %s", model, typ, tier, inputPath)
}

func human(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	default:
		return strconv.FormatInt(n, 10)
	}
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func humanSecs(s int) string {
	if s >= 60 {
		return fmt.Sprintf("%dm", s/60)
	}
	return fmt.Sprintf("%ds", s)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func oneArg(cmd string, args []string) string {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fatalf("usage: cx %s <job_id>", cmd)
	}
	return args[0]
}

func readInput(path string) []byte {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatalf("reading stdin: %v", err)
		}
		return data
	}
	return readFile(path)
}

func readFile(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		fatalf("reading %q: %v", path, err)
	}
	return data
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		fatalf("encoding request: %v", err)
	}
	return b
}

func printJSON(b []byte) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		os.Stdout.Write(b)
		if len(b) > 0 && b[len(b)-1] != '\n' {
			fmt.Println()
		}
		return
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(out))
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "cx: "+format+"\n", a...)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `cx  -  Computexchange buyer CLI

Usage:
  cx quote    --model <id> --type <jobtype> [--input <file|->] [--tier t] [--json]
  cx submit   --model <id> --type <jobtype> [--input <file|->] [--quote-id q_…] [--max-usd F] [flags] [--wait]
  cx status   <job_id>
  cx results  <job_id>
  cx invoice  <job_id> [--json]
  cx events   <job_id>
  cx failures <job_id>
  cx cancel   <job_id>
  cx models
  cx estimate --model <id> --units N [--tier t]
  cx explain-scheduler --worker <id>   (admin key)
  cx audit codebase [--out DIR]        (authoritative census; retires make loc)
  cx source-id [--root DIR] [--field F] (source fingerprint; replaces scripts/source_fingerprint.py)
  cx verify --ledger PATH [flags]      (validate a prove-local ledger; replaces verify_proof_ledger.py)
  cx version [--json]

Env:
  CX_API_URL   control plane base URL (default http://localhost:8080)
  CX_API_KEY   buyer api key (sent as Authorization: Bearer)

Job types: embed, batch_infer
Run "cx submit -h" for the full flag list.
`)
}
