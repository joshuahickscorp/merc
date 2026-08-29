package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OpenAI-compatible streaming client used by the serving matrix harness.
//
// Speaks the same chat/completions shape as ops/scripts/runtime-parity-sweep.py and
// ops/scripts/gateway-parity.py so a receipt from this harness is comparable to
// those. Does not vendor any engine; the upstream is an HTTP endpoint only.
//
// Licence note: this client reimplements only the public OpenAI HTTP wire shape
// (request/response JSON + SSE). No SGLang, TensorRT-LLM, LMDeploy or vLLM
// source is copied. Those engines' own licences (Apache-2.0 for SGLang,
// TensorRT-LLM and LMDeploy; Apache-2.0 for vLLM) apply to their processes,
// not to this file.

// ServingEngineClient is what the harness needs from one live endpoint.
type ServingEngineClient interface {
	// CompleteOne runs one streaming completion and returns timing samples.
	CompleteOne(ctx context.Context, prompt string, maxTokens int) (ServingRequestSample, error)
}

// ServingRequestSample is one completed request's measured path.
type ServingRequestSample struct {
	TTFTMs           float64
	TotalMs          float64
	ITLMs            []float64
	PromptTokens     int
	CompletionTokens int
	Err              string
}

// OpenAICompatClient hits an OpenAI-compatible /v1/chat/completions endpoint.
type OpenAICompatClient struct {
	BaseURL    string // e.g. http://127.0.0.1:8080/v1
	APIKey     string
	Model      string
	HTTPClient *http.Client
	UserAgent  string
}

func (c *OpenAICompatClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (c *OpenAICompatClient) CompleteOne(ctx context.Context, prompt string, maxTokens int) (ServingRequestSample, error) {
	body := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens":  maxTokens,
		"temperature": 0,
		"stream":      true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ServingRequestSample{}, err
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return ServingRequestSample{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	ua := c.UserAgent
	if ua == "" {
		ua = "merc-serving-matrix/1.0"
	}
	req.Header.Set("User-Agent", ua)
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	start := time.Now()
	resp, err := c.client().Do(req)
	if err != nil {
		return ServingRequestSample{Err: err.Error()}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		msg := fmt.Sprintf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		return ServingRequestSample{Err: msg}, fmt.Errorf("%s", msg)
	}

	var (
		ttftMs           *float64
		tokenTimes       []time.Time
		completionTokens int
		promptTokens     int
	)
	scanner := bufio.NewScanner(resp.Body)
	// SSE lines can be large for big chunks.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(line[6:])
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
				PromptTokens     int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			now := time.Now()
			tokenTimes = append(tokenTimes, now)
			if ttftMs == nil {
				v := float64(now.Sub(start).Microseconds()) / 1000.0
				ttftMs = &v
			}
		}
		if chunk.Usage != nil {
			if chunk.Usage.CompletionTokens > 0 {
				completionTokens = chunk.Usage.CompletionTokens
			}
			if chunk.Usage.PromptTokens > 0 {
				promptTokens = chunk.Usage.PromptTokens
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return ServingRequestSample{Err: err.Error()}, err
	}
	totalMs := float64(time.Since(start).Microseconds()) / 1000.0
	var itl []float64
	for i := 1; i < len(tokenTimes); i++ {
		itl = append(itl, float64(tokenTimes[i].Sub(tokenTimes[i-1]).Microseconds())/1000.0)
	}
	sample := ServingRequestSample{
		TotalMs:          totalMs,
		ITLMs:            itl,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
	}
	if ttftMs != nil {
		sample.TTFTMs = *ttftMs
	}
	return sample, nil
}

// ServingMatrixPromptCorpus builds a steady-state prompt list for one point.
//
// Length is approximate: the harness counts prompts (requests), not tokenizer
// tokens, for the 5× rule. PromptTokens on the point is a target size; the
// corpus pads a fixed base string to roughly that character length so engines
// see similar prefill cost without requiring a local tokenizer.
func ServingMatrixPromptCorpus(point ServingMatrixPoint) []string {
	n := RequiredPromptCount(point.Concurrency)
	base := "Write a short factual paragraph about the water cycle. Be specific and do not repeat yourself. "
	// ~4 chars/token is a rough English average; pad to approximate the target.
	targetChars := point.PromptTokens * 4
	if targetChars < len(base) {
		targetChars = len(base)
	}
	pad := strings.Repeat("detail ", (targetChars-len(base))/7+1)
	body := (base + pad)
	if len(body) > targetChars {
		body = body[:targetChars]
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		// Slight per-prompt variation so pure prefix caching does not collapse
		// every warm request into a free hit unless the state is prefix-hit.
		out[i] = fmt.Sprintf("%s [corpus=%d]", body, i)
	}
	return out
}

// RunServingMatrixPoint executes one arm at one point against a live client.
//
// Enforces the 5× steady-state rule and per-point refusal. Returns a cell
// result that is MEASURED, REFUSED or INCOMPARABLE — never a silent skip.
func RunServingMatrixPoint(
	ctx context.Context,
	arm ServingMatrixArm,
	point ServingMatrixPoint,
	client ServingEngineClient,
) ServingMatrixCellResult {
	key := ArmKey(arm)
	result := ServingMatrixCellResult{ArmKey: key, Point: point}

	if reason := RefuseMatrixPoint(arm, point); reason != "" {
		result.Status = "REFUSED"
		result.Reason = reason
		return result
	}

	prompts := ServingMatrixPromptCorpus(point)
	if reason := RefuseSubSteadyState(point.Concurrency, len(prompts)); reason != "" {
		result.Status = "INCOMPARABLE"
		result.Reason = reason
		return result
	}

	// Warm path for non-cold states: one discarded request so subsequent work
	// is not dominated by model load. Cold deliberately skips this.
	if point.State != "cold" && client != nil {
		warmTok := point.OutputTokens
		if warmTok > 8 {
			warmTok = 8
		}
		_, _ = client.CompleteOne(ctx, prompts[0], warmTok)
	}
	// prefix-hit: if the arm claimed support, fire a shared-prefix warm-up.
	// (Support was already checked by RefuseMatrixPoint.)
	if point.State == "prefix-hit" && client != nil {
		warmTok := point.OutputTokens
		if warmTok > 8 {
			warmTok = 8
		}
		_, _ = client.CompleteOne(ctx, prompts[0], warmTok)
	}

	if client == nil {
		result.Status = "REFUSED"
		result.Reason = "refused: no live client bound for engine " + arm.Engine
		return result
	}

	type outcome struct {
		sample ServingRequestSample
		err    error
	}
	outcomes := make([]outcome, len(prompts))
	sem := make(chan struct{}, point.Concurrency)
	var wg sync.WaitGroup
	wallStart := time.Now()
	for i, prompt := range prompts {
		wg.Add(1)
		go func(i int, prompt string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// Per-request timeout bounded by context.
			sample, err := client.CompleteOne(ctx, prompt, point.OutputTokens)
			outcomes[i] = outcome{sample: sample, err: err}
		}(i, prompt)
	}
	wg.Wait()
	wall := time.Since(wallStart).Seconds()

	var (
		ttfts             []float64
		tpots             []float64
		itls              []float64
		totalIn, totalOut int
		ok, errs          int
	)
	for _, o := range outcomes {
		if o.err != nil || o.sample.Err != "" {
			errs++
			continue
		}
		ok++
		if o.sample.TTFTMs > 0 {
			ttfts = append(ttfts, o.sample.TTFTMs)
		}
		if o.sample.CompletionTokens > 1 && o.sample.TotalMs > o.sample.TTFTMs {
			// TPOT ≈ (total − ttft) / (completion_tokens − 1)
			tpot := (o.sample.TotalMs - o.sample.TTFTMs) / float64(o.sample.CompletionTokens-1)
			if tpot > 0 {
				tpots = append(tpots, tpot)
			}
		}
		itls = append(itls, o.sample.ITLMs...)
		totalIn += o.sample.PromptTokens
		totalOut += o.sample.CompletionTokens
	}

	if ok == 0 {
		result.Status = "REFUSED"
		result.Reason = fmt.Sprintf("refused: all %d requests failed at this point", len(prompts))
		return result
	}

	metrics := DeriveMetricsFromSamples(ttfts, tpots, itls, totalIn, totalOut, ok, len(prompts), errs, wall, arm)
	result.Status = "MEASURED"
	result.Metrics = &metrics
	return result
}
