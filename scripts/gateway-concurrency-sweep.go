// Gateway-only concurrency sweep against a synthetic streaming stand-in.
//
// Measures the cost attributable to the control plane's upstream HTTP client
// (connection reuse under concurrent streaming), not CUDA engine throughput.
// Labelled gateway-only: the stand-in is not a model.
//
//	go run scripts/gateway-concurrency-sweep.go \
//	  -out evidence/perf/gateway-concurrency-sweep.json
//
// Compares DefaultTransport (MaxIdleConnsPerHost=2) against the transport
// newRealtimeHTTPClient builds. Also records TCP connections opened to the
// stand-in and how many requests each connection served.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Mirrors control/realtime.go:realtimeMaxIdleConnsPerHost so a drift is visible
// when reading the receipt next to the source. Keep in sync.
const realtimeMaxIdleConnsPerHost = 128

type connTracker struct {
	mu       sync.Mutex
	newConns int
	// remote -> request count served on that TCP connection
	perConn map[string]int
	active  int
	peak    int
}

func (t *connTracker) onState(c net.Conn, s http.ConnState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := c.RemoteAddr().String()
	switch s {
	case http.StateNew:
		t.newConns++
		if t.perConn == nil {
			t.perConn = make(map[string]int)
		}
		if _, ok := t.perConn[key]; !ok {
			t.perConn[key] = 0
		}
	case http.StateActive:
		t.active++
		if t.active > t.peak {
			t.peak = t.active
		}
		if t.perConn == nil {
			t.perConn = make(map[string]int)
		}
		t.perConn[key]++
	case http.StateIdle, http.StateClosed, http.StateHijacked:
		if t.active > 0 {
			// Leave active counting approximate; peak is what we need.
		}
	}
}

func (t *connTracker) snapshot() (newConns, peak int, reqsPerConn []int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, n := range t.perConn {
		reqsPerConn = append(reqsPerConn, n)
	}
	sort.Ints(reqsPerConn)
	return t.newConns, t.peak, reqsPerConn
}

func (t *connTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.newConns = 0
	t.perConn = make(map[string]int)
	t.active = 0
	t.peak = 0
}

func startStandIn(tokenCount int, interTokenDelay time.Duration) (*http.Server, *connTracker, string, error) {
	tracker := &connTracker{perConn: make(map[string]int)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, "", err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Drain body so the client can reuse the connection cleanly.
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		// First token promptly (TTFT from stand-in is ~0 beyond network).
		for i := 0; i < tokenCount; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"standin\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}],\"usage\":null}\n\n")
			flusher.Flush()
			if interTokenDelay > 0 {
				time.Sleep(interTokenDelay)
			}
		}
		fmt.Fprintf(w, "data: {\"id\":\"standin\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", tokenCount, 8+tokenCount)
		flusher.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"standin-stream"}]}`)
	})
	srv := &http.Server{
		Handler:   mux,
		ConnState: tracker.onState,
	}
	go func() { _ = srv.Serve(ln) }()
	return srv, tracker, "http://" + ln.Addr().String(), nil
}

type dialCounter struct {
	base  *http.Transport
	dials atomic.Int64
}

func (d *dialCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	return d.base.RoundTrip(req)
}

func transportWithDialCount(src *http.Transport, dialDelay time.Duration) (*http.Transport, *atomic.Int64) {
	cl := src.Clone()
	var dials atomic.Int64
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	prev := cl.DialContext
	if prev == nil {
		prev = d.DialContext
	}
	cl.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		// Optional synthetic dial RTT so localhost can expose the same
		// TTFT shape a remote GPU pod would show when connections are not
		// reused. Zero means pure loopback dial cost (sub-millisecond).
		if dialDelay > 0 {
			timer := time.NewTimer(dialDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		return prev(ctx, network, addr)
	}
	return cl, &dials
}

// defaultLikeTransport is what newRealtimeHTTPClient used before the fix:
// http.DefaultTransport with its unwritten MaxIdleConnsPerHost=2.
func defaultLikeTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Explicitly restore the defaults we are measuring against so a local
	// mutation of DefaultTransport cannot hide the tax.
	tr.MaxIdleConnsPerHost = 2 // http.DefaultMaxIdleConnsPerHost
	tr.MaxIdleConns = 100      // Transport.MaxIdleConns default
	return tr
}

// fixedTransport mirrors the production fix in control/realtime.go.
func fixedTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = realtimeMaxIdleConnsPerHost
	tr.MaxIdleConns = realtimeMaxIdleConnsPerHost * 4
	return tr
}

type levelResult struct {
	Concurrency          int     `json:"concurrency"`
	RequestsAttempted    int     `json:"requests_attempted"`
	RequestsOK           int     `json:"requests_ok"`
	Errors               int     `json:"errors"`
	WallSeconds          float64 `json:"wall_seconds"`
	CompletedPerSecond   float64 `json:"completed_requests_per_second"`
	TTFT                 statsMS `json:"ttft_ms"`
	TCPConnectionsOpened int     `json:"tcp_connections_opened"`
	PeakActiveConns      int     `json:"peak_active_connections"`
	ClientDials          int64   `json:"client_dials"`
	DialsPerOKRequest    float64 `json:"dials_per_ok_request"`
	ReqsPerConnP50       float64 `json:"requests_per_connection_p50"`
	ReqsPerConnMean      float64 `json:"requests_per_connection_mean"`
	ReqsPerConnMax       int     `json:"requests_per_connection_max"`
	ConnectionCount      int     `json:"connection_count"`
}

type statsMS struct {
	Samples int     `json:"samples"`
	MeanMS  float64 `json:"mean_ms"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	P99MS   float64 `json:"p99_ms"`
	MinMS   float64 `json:"min_ms"`
	MaxMS   float64 `json:"max_ms"`
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	idx := int(float64(len(cp)) * p)
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func statsOf(ms []float64) statsMS {
	if len(ms) == 0 {
		return statsMS{}
	}
	sum := 0.0
	minV, maxV := ms[0], ms[0]
	for _, v := range ms {
		sum += v
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	return statsMS{
		Samples: len(ms),
		MeanMS:  round3(sum / float64(len(ms))),
		P50MS:   round3(percentile(ms, 0.50)),
		P95MS:   round3(percentile(ms, 0.95)),
		P99MS:   round3(percentile(ms, 0.99)),
		MinMS:   round3(minV),
		MaxMS:   round3(maxV),
	}
}

func round3(v float64) float64 { return float64(int(v*1000+0.5)) / 1000 }

func runLevel(base string, tr *http.Transport, tracker *connTracker, concurrency, nReq int, dialDelay time.Duration) levelResult {
	tracker.reset()
	tr.CloseIdleConnections()
	measured, dials := transportWithDialCount(tr, dialDelay)
	client := &http.Client{Transport: measured, Timeout: 120 * time.Second}

	// Warm one connection so process-level first-dial noise is not billed to
	// the level, then drop idle so the level measures its own reuse behaviour.
	_, _ = doRequest(client, base)
	measured.CloseIdleConnections()
	dials.Store(0)
	tracker.reset()

	var (
		wg    sync.WaitGroup
		sem   = make(chan struct{}, concurrency)
		mu    sync.Mutex
		ttfts []float64
		okN   int
		errN  int
	)
	start := time.Now()
	for i := 0; i < nReq; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ttft, err := doRequest(client, base)
			mu.Lock()
			if err != nil {
				errN++
			} else {
				okN++
				ttfts = append(ttfts, float64(ttft.Microseconds())/1000.0)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	wall := time.Since(start)
	newConns, peak, reqsPer := tracker.snapshot()
	meanRPC := 0.0
	maxRPC := 0
	sumRPC := 0
	for _, n := range reqsPer {
		sumRPC += n
		if n > maxRPC {
			maxRPC = n
		}
	}
	if len(reqsPer) > 0 {
		meanRPC = float64(sumRPC) / float64(len(reqsPer))
	}
	p50RPC := 0.0
	if len(reqsPer) > 0 {
		p50RPC = float64(reqsPer[len(reqsPer)/2])
	}
	rps := 0.0
	if wall > 0 {
		rps = float64(okN) / wall.Seconds()
	}
	dialsPer := 0.0
	if okN > 0 {
		dialsPer = float64(dials.Load()) / float64(okN)
	}
	return levelResult{
		Concurrency:          concurrency,
		RequestsAttempted:    nReq,
		RequestsOK:           okN,
		Errors:               errN,
		WallSeconds:          round3(wall.Seconds()),
		CompletedPerSecond:   round3(rps),
		TTFT:                 statsOf(ttfts),
		TCPConnectionsOpened: newConns,
		PeakActiveConns:      peak,
		ClientDials:          dials.Load(),
		DialsPerOKRequest:    round3(dialsPer),
		ReqsPerConnP50:       p50RPC,
		ReqsPerConnMean:      round3(meanRPC),
		ReqsPerConnMax:       maxRPC,
		ConnectionCount:      len(reqsPer),
	}
}

func doRequest(client *http.Client, base string) (time.Duration, error) {
	body := strings.NewReader(`{"model":"standin","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16,"temperature":0}`)
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/v1/chat/completions", body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer standin")
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	br := bufio.NewReader(resp.Body)
	var ttft time.Duration
	gotFirst := false
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && !gotFirst {
			// First non-empty read is first SSE bytes.
			if strings.HasPrefix(string(line), "data:") || strings.TrimSpace(string(line)) != "" {
				ttft = time.Since(t0)
				gotFirst = true
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return ttft, err
		}
	}
	if !gotFirst {
		return 0, fmt.Errorf("empty stream")
	}
	return ttft, nil
}

func gitHEAD() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func main() {
	outPath := flag.String("out", "evidence/perf/gateway-concurrency-sweep.json", "receipt path")
	tokenCount := flag.Int("tokens", 16, "stand-in completion tokens per request")
	interTokenMS := flag.Int("inter-token-ms", 3, "stand-in inter-token delay (ms)")
	// Remote GPU pods pay tens of ms per cold dial (TCP+TLS+handshake). Loopback
	// hides that; 15ms is a conservative same-region proxy for the cost the
	// idle-pool miss actually bills into TTFT on a real pod. Set 0 for pure
	// loopback (connection counts still expose the bug).
	dialDelayMS := flag.Int("dial-delay-ms", 15, "synthetic dial RTT added on each TCP dial (0=loopback only)")
	flag.Parse()

	levels := []int{1, 2, 4, 8, 16, 32, 64}
	inter := time.Duration(*interTokenMS) * time.Millisecond
	dialDelay := time.Duration(*dialDelayMS) * time.Millisecond
	srv, tracker, base, err := startStandIn(*tokenCount, inter)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer srv.Close()

	type arm struct {
		Name string
		Tr   *http.Transport
	}
	arms := []arm{
		{Name: "default_transport_max_idle_per_host_2", Tr: defaultLikeTransport()},
		{Name: "fixed_transport_max_idle_per_host_128", Tr: fixedTransport()},
	}

	results := map[string]map[string]levelResult{}
	for _, arm := range arms {
		results[arm.Name] = map[string]levelResult{}
		fmt.Fprintf(os.Stderr, "=== %s ===\n", arm.Name)
		for _, c := range levels {
			nReq := c * 5 // SGLang guidance: ≥5× concurrency so steady state is real
			if nReq < 20 {
				nReq = 20 // keep p95/p99 meaningful at low c
			}
			lr := runLevel(base, arm.Tr, tracker, c, nReq, dialDelay)
			results[arm.Name][fmt.Sprintf("%d", c)] = lr
			fmt.Fprintf(os.Stderr, "c=%2d ok=%3d/%3d dials=%3d conns=%3d dials/ok=%.2f ttft_p50=%.2fms p95=%.2fms rps=%.1f wall=%.2fs\n",
				c, lr.RequestsOK, lr.RequestsAttempted, lr.ClientDials, lr.TCPConnectionsOpened,
				lr.DialsPerOKRequest, lr.TTFT.P50MS, lr.TTFT.P95MS, lr.CompletedPerSecond, lr.WallSeconds)
		}
	}

	// Gateway-attributable tax: dials/ok and TTFT growth of default vs fixed.
	// Flat dials/ok≈1/c-window means reuse works; dials/ok≈1 means no reuse.
	receipt := map[string]any{
		"schema_version":     1,
		"kind":               "gateway_concurrency_sweep",
		"measured_at":        time.Now().UTC().Format(time.RFC3339),
		"merc_source_commit": gitHEAD(),
		"scope": map[string]any{
			"upstream":             "synthetic_streaming_stand_in",
			"upstream_label":       "gateway-only synthetic SSE stand-in (not a model; not llama-server)",
			"token_count":          *tokenCount,
			"inter_token_delay_ms": *interTokenMS,
			"dial_delay_ms":        *dialDelayMS,
			"dial_delay_note":      "synthetic per-dial RTT to make remote-pod cold-dial cost visible on loopback; connection counts are the primary evidence and do not depend on this",
			"concurrency_levels":   levels,
			"requests_per_level":   "max(20, 5*concurrency)",
			"stream":               true,
			"temperature":          0,
			"measurement_target":   "control-plane upstream http.Client / http.Transport idle pool (newRealtimeHTTPClient)",
			"arms": []string{
				"default_transport_max_idle_per_host_2",
				"fixed_transport_max_idle_per_host_128",
			},
		},
		"root_cause": map[string]any{
			"file_line":       "control/realtime.go:newRealtimeHTTPClient",
			"mechanism":       "http.DefaultTransport uses DefaultMaxIdleConnsPerHost=2. Concurrent streaming holds one connection per in-flight request; when a stream ends only 2 idle connections are retained per upstream host, so the next wave redials. Free at c=1 (single connection reused); expensive at c=32 (≈30 of 32 requests pay a cold dial).",
			"evidence_metric": "dials_per_ok_request and tcp_connections_opened scale with concurrency under the default arm and stay near 1 connection-wave under the fixed arm",
		},
		"proves":         "Gateway upstream-client connection-reuse overhead under a synthetic streaming load. Side-by-side default vs fixed transport at every concurrency level.",
		"does_not_prove": "Does not prove the c=32 CUDA throughput deficit on a GPU pod is closed. Does not measure AuthorizeRealtimeContract, buyer funding locks, SSE evidence hashing, or any live engine. Stand-in latency is artificial.",
		"levels":         results,
	}

	// Summary deltas at key levels for the human reader.
	summary := map[string]any{}
	for _, c := range []int{1, 8, 32, 64} {
		key := fmt.Sprintf("%d", c)
		def := results["default_transport_max_idle_per_host_2"][key]
		fix := results["fixed_transport_max_idle_per_host_128"][key]
		summary[key] = map[string]any{
			"default_dials_per_ok": def.DialsPerOKRequest,
			"fixed_dials_per_ok":   fix.DialsPerOKRequest,
			"default_ttft_p50_ms":  def.TTFT.P50MS,
			"fixed_ttft_p50_ms":    fix.TTFT.P50MS,
			"default_ttft_p95_ms":  def.TTFT.P95MS,
			"fixed_ttft_p95_ms":    fix.TTFT.P95MS,
			"default_conns":        def.TCPConnectionsOpened,
			"fixed_conns":          fix.TCPConnectionsOpened,
			"default_rps":          def.CompletedPerSecond,
			"fixed_rps":            fix.CompletedPerSecond,
		}
	}
	receipt["summary"] = summary

	raw, err := json.Marshal(receipt)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tmp, err := os.CreateTemp("", "merc-concurrency-payload-*.json")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := tmp.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Route through the single bound-evidence writer (identity + sticky withdrawal).
	cmd := exec.Command("python3", "scripts/write-bound-evidence.py",
		"--out", *outPath,
		"--harness", "scripts/gateway-concurrency-sweep.go",
		"--payload-file", tmpName,
		"--build-binary", "scripts/gateway-concurrency-sweep.go",
		"--exact-config", "embedded concurrency sweep config",
		"--raw-samples", "embedded per-level samples",
		"--model-na", "concurrency sweep; model digests not remeasured here",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "bound evidence write refused:", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *outPath)
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}
