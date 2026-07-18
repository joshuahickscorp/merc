// cx audit codebase — the authoritative census.
//
// This RETIRES `make loc` (which only counted agent/src + control *.rs/*.go).
// It walks the git-tracked file set (respecting .gitignore) and classifies
// every file by language, subsystem, layer, and ship/generated/vendored/pack
// status, then emits a deterministic set of census artifacts. It never mutates
// the tree and takes no wall-clock input, so `cx audit codebase` on a fixed
// commit is reproducible bit-for-bit.
//
//	cx audit codebase [--out DIR]   (default DIR: census/)
//
// Outputs (in DIR):
//
//	CODEBASE_CENSUS.json      per-file records + aggregates
//	CODEBASE_CENSUS.md        human-readable summary
//	CODEBASE_LOC.json         LOC rollups (language / subsystem / layer)
//	CODEBASE_BYTES.json       byte rollups + largest tracked files
//	PYTHON_RECLAMATION.json   every .py classified (no "unknown" allowed)
//	DEPENDENCY_CENSUS.json    go / cargo / python / node dependency counts
//	CONDENSATION_CANDIDATES.json   ranked reduction candidates
//
// LIVE_BOUND_NO_TOUCH.json and PERFORMANCE_BASELINE.json are authored by the
// operator (they encode running-service investigation and measured build/run
// numbers the census cannot infer) and are referenced, not generated, here.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// ---- per-file record ----

type fileRecord struct {
	Path           string `json:"path"`
	Language       string `json:"language"`
	Subsystem      string `json:"subsystem"`
	Layer          string `json:"layer"`
	Classification string `json:"classification"`
	Binary         bool   `json:"binary"`
	LOC            int    `json:"loc"`
	Code           int    `json:"code"`
	Blank          int    `json:"blank"`
	Comment        int    `json:"comment"`
	Bytes          int64  `json:"bytes"`
	Generated      bool   `json:"generated"`
	Vendored       bool   `json:"vendored"`
	Shipped        bool   `json:"shipped"`
	PackEligible   string `json:"pack_eligible,omitempty"`
	PythonPlan     string `json:"python_plan,omitempty"`
	LastCommit     string `json:"last_commit,omitempty"`
	DeletionRisk   string `json:"deletion_risk"`
}

func cmdAudit(args []string) {
	if len(args) == 0 || args[0] != "codebase" {
		fatalf("usage: cx audit codebase [--out DIR]")
	}
	out := "census"
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--out":
			if i+1 >= len(args) {
				fatalf("--out needs a directory")
			}
			out = args[i+1]
			i++
		default:
			fatalf("unknown flag %q", args[i])
		}
	}

	root := repoRoot()
	files := gitTracked(root)
	last := lastCommitDates(root)
	head := gitHead(root)

	records := make([]fileRecord, 0, len(files))
	for _, rel := range files {
		abs := filepath.Join(root, rel)
		info, err := os.Stat(abs)
		if err != nil {
			continue // tracked-but-absent (submodule/gitlink) — skip
		}
		rec := fileRecord{
			Path:       rel,
			Language:   languageOf(rel),
			Subsystem:  subsystemOf(rel),
			Bytes:      info.Size(),
			LastCommit: last[rel],
		}
		rec.Vendored = strings.Contains(rel, "/vendor/") || strings.HasPrefix(rel, "vendor/")
		data := readFileMaybe(abs)
		rec.Binary = looksBinary(rel, data)
		if !rec.Binary {
			rec.LOC, rec.Code, rec.Blank, rec.Comment = countLOC(data, rec.Language)
		}
		rec.Generated = isGenerated(rel, data)
		rec.PackEligible = packOf(rel)
		rec.Classification = classify(rel, rec)
		rec.Shipped = shipped(rel, rec)
		rec.Layer = layerOf(rel, rec)
		rec.DeletionRisk = deletionRisk(rel, rec)
		if rec.Language == "python" {
			rec.PythonPlan = pythonPlan(rel, data)
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })

	if err := os.MkdirAll(filepath.Join(root, out), 0o755); err != nil {
		fatalf("mkdir %s: %v", out, err)
	}
	writeCensus(root, out, head, records)
	writeLOC(root, out, records)
	writeBytes(root, out, records)
	writePython(root, out, records)
	writeDeps(root, out)
	writeCandidates(root, out, records)

	fmt.Printf("cx audit codebase — %d tracked files, HEAD %s\n", len(records), short(head))
	fmt.Printf("artifacts written to %s/\n", out)
}

// ---- git helpers ----

func repoRoot() string {
	o, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fatalf("not a git repo: %v", err)
	}
	return strings.TrimSpace(string(o))
}

func gitHead(root string) string {
	o, err := runIn(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(o)
}

func gitTracked(root string) []string {
	o, err := runIn(root, "git", "ls-files", "-z")
	if err != nil {
		fatalf("git ls-files: %v", err)
	}
	parts := strings.Split(strings.TrimRight(o, "\x00"), "\x00")
	files := parts[:0]
	for _, p := range parts {
		if p != "" {
			files = append(files, p)
		}
	}
	return files
}

// lastCommitDates returns the most-recent commit ISO date touching each file,
// computed in a single `git log` pass (1 process, not one-per-file).
func lastCommitDates(root string) map[string]string {
	out := map[string]string{}
	o, err := runIn(root, "git", "log", "--no-merges", "--name-only", "--format=%x01%cI")
	if err != nil {
		return out
	}
	cur := ""
	for _, line := range strings.Split(o, "\n") {
		if strings.HasPrefix(line, "\x01") {
			cur = strings.TrimPrefix(line, "\x01")
			continue
		}
		if line == "" || cur == "" {
			continue
		}
		if _, seen := out[line]; !seen { // first (newest) wins
			out[line] = cur
		}
	}
	return out
}

func runIn(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	err := cmd.Run()
	return buf.String(), err
}

func readFileMaybe(abs string) []byte {
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	return b
}

// ---- classification ----

var extLang = map[string]string{
	".go": "go", ".rs": "rust", ".py": "python", ".swift": "swift",
	".js": "javascript", ".mjs": "javascript", ".ts": "typescript",
	".sql": "sql", ".metal": "shader", ".sh": "shell", ".bash": "shell",
	".md": "documentation", ".json": "json", ".jsonl": "json",
	".yml": "yaml", ".yaml": "yaml", ".toml": "toml", ".proto": "proto",
	".html": "web", ".css": "web", ".svg": "web", ".webmanifest": "web",
	".xml": "config", ".plist": "config", ".sb": "config",
	".txt": "text", ".rtf": "text", ".patch": "patch", ".orig": "patch",
	".png": "binary", ".jpg": "binary", ".jpeg": "binary", ".webp": "binary",
	".pdf": "binary", ".glb": "binary", ".gltf": "binary", ".onnx": "binary",
	".pt": "binary", ".woff2": "binary", ".ttf": "binary", ".ico": "binary",
	".br": "binary", ".mp4": "binary", ".wasm": "binary", ".gz": "binary",
	".lock": "lockfile", ".sum": "lockfile", ".resolved": "lockfile",
	".mod": "config", ".dockerignore": "config", ".gitignore": "config",
}

func languageOf(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if l, ok := extLang[ext]; ok {
		return l
	}
	base := filepath.Base(path)
	switch base {
	case "Dockerfile", "Makefile", "Caddyfile":
		return "config"
	case "go.mod", "go.sum", "Cargo.toml", "Cargo.lock":
		return "config"
	}
	if ext == "" && looksScript(base) {
		return "shell"
	}
	return "other"
}

func looksScript(base string) bool {
	return base == "cx" || base == "vllm" || base == "online"
}

func subsystemOf(path string) string {
	if strings.Contains(path, "/vendor/") || strings.HasPrefix(path, "vendor/") {
		return "vendored"
	}
	top := firstElem(path)
	switch top {
	case "control":
		base := filepath.Base(path)
		switch {
		case containsAny(base, "billing", "payout", "ledger", "stripe", "payment", "invoice", "refund", "dispute", "charge", "hold", "settle"):
			return "payment"
		case containsAny(base, "verif", "reputation", "honeypot", "fraud", "redundan"):
			return "verification"
		default:
			return "control"
		}
	case "cli":
		return "interface"
	case "agent":
		return "agent"
	case "spec-engine", "token-spec-poc":
		return "speculation"
	case "proof":
		return "proof"
	case "proto":
		return "contract"
	case "sdk":
		return "sdk"
	case "web", "macapp", "logo":
		return "interface"
	case "render", "renderer":
		return "render"
	case "db":
		return "store"
	case "scripts":
		if strings.HasPrefix(path, "scripts/spec-lab/") {
			return "speculation"
		}
		return "proof"
	case "docs":
		return "documentation"
	case "monitoring", "docker", "patches":
		return "ops"
	default:
		return "ops"
	}
}

// isGenerated flags deterministically-regenerable / machine-emitted files.
func isGenerated(path string, data []byte) bool {
	base := filepath.Base(path)
	switch {
	case strings.HasSuffix(path, ".br"): // brotli-precompressed asset
		return true
	case strings.Contains(base, ".min."):
		return true
	case strings.HasPrefix(path, "web/assets/site/vendor/"): // vendored three.js etc
		return true
	}
	// header marker in first 2 lines
	head := data
	if len(head) > 400 {
		head = head[:400]
	}
	h := strings.ToLower(string(head))
	if strings.Contains(h, "code generated") || strings.Contains(h, "do not edit") || strings.Contains(h, "@generated") || strings.Contains(h, "autogenerated") {
		return true
	}
	return false
}

// packOf returns the optional-pack name a file should relocate into, or "".
func packOf(path string) string {
	switch {
	case strings.HasPrefix(path, "render/") && !strings.HasPrefix(path, "render/site/downsize"):
		return "computexchange-render-lab"
	case strings.HasPrefix(path, "renderer/"):
		return "computexchange-render-lab"
	case strings.HasPrefix(path, "scripts/spec-lab/"):
		return "computexchange-render-lab"
	case strings.HasPrefix(path, "docs/") && isArchiveDoc(path):
		return "computexchange-docs-archive"
	case strings.HasPrefix(path, "docs/bench-local-reports/") || strings.HasPrefix(path, "docs/speed-lane-reports/"):
		return "computexchange-benchmarks"
	case strings.HasPrefix(path, "proof/") && strings.Contains(path, "archive"):
		return "computexchange-proof-archive"
	}
	return ""
}

func isArchiveDoc(path string) bool {
	return strings.HasPrefix(path, "docs/internal/") ||
		strings.Contains(path, "REPORT") ||
		strings.Contains(path, "-report") ||
		strings.Contains(path, "AUDIT") ||
		strings.Contains(path, "ROADMAP")
}

func classify(path string, r fileRecord) string {
	base := filepath.Base(path)
	switch {
	case r.Vendored:
		return "vendored"
	case r.Binary:
		return "binary-asset"
	case r.Generated:
		return "generated"
	case r.Language == "documentation":
		return "doc"
	case strings.Contains(base, "_test.") || strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.go") || strings.Contains(path, "/tests/") || strings.Contains(base, ".test."):
		return "test"
	case r.Language == "json" && (strings.Contains(path, "fixture") || strings.Contains(path, "golden") || strings.HasSuffix(path, ".jsonl")):
		return "fixture"
	case containsAny(r.Language, "yaml", "toml", "config", "lockfile"):
		return "config"
	case r.Language == "json":
		return "data"
	case containsAny(r.Language, "go", "rust", "python", "swift", "javascript", "typescript", "sql", "shader", "proto", "shell", "web"):
		return "source"
	default:
		return "other"
	}
}

func shipped(path string, r fileRecord) bool {
	if r.PackEligible != "" || r.Classification == "doc" {
		return false
	}
	top := firstElem(path)
	switch top {
	case "control", "agent", "cli", "spec-engine", "token-spec-poc", "proto", "db", "sdk", "macapp", "monitoring", "docker":
		return r.Classification != "test"
	case "web":
		return !strings.HasPrefix(path, "web/assets/site/vendor/")
	case "proof":
		return true // trust boundary ships
	}
	return false
}

func layerOf(path string, r fileRecord) string {
	if r.Vendored {
		return "vendored"
	}
	if r.Generated {
		return "generated"
	}
	if r.PackEligible != "" {
		return "pack:" + r.PackEligible
	}
	top := firstElem(path)
	kernelDir := top == "control" || top == "agent" || top == "spec-engine" ||
		top == "token-spec-poc" || top == "cli" || top == "proto" || top == "db"
	codeLang := containsAny(r.Language, "go", "rust", "sql", "proto", "shader")
	if kernelDir && codeLang && r.Classification == "source" {
		return "kernel"
	}
	if r.Classification == "test" && kernelDir {
		return "active_product"
	}
	switch top {
	case "sdk", "web", "macapp":
		return "active_product"
	case "docs":
		return "historical"
	case "scripts", "monitoring", "docker", "patches", "logo":
		return "active_repo_support"
	}
	if kernelDir {
		return "active_repo_support"
	}
	return "active_repo_support"
}

func deletionRisk(path string, r fileRecord) string {
	if r.Layer == "kernel" || r.Subsystem == "payment" || r.Subsystem == "verification" {
		return "high"
	}
	if r.Classification == "test" || r.Classification == "config" || r.Subsystem == "contract" || r.Subsystem == "store" {
		return "high"
	}
	if r.PackEligible != "" || r.Classification == "doc" || r.Binary {
		return "low"
	}
	if r.Vendored {
		return "medium"
	}
	return "medium"
}

// pythonPlan classifies every .py into a reclamation bucket. No file is left
// "unknown": the final default routes by directory.
func pythonPlan(path string, data []byte) string {
	body := string(data)
	usesBlender := strings.Contains(body, "import bpy") || strings.Contains(body, "import bmesh") || strings.Contains(body, "bpy.")
	base := filepath.Base(path)
	switch {
	case strings.HasPrefix(path, "sdk/python/"):
		if strings.HasPrefix(base, "test_") || strings.Contains(base, "_test") {
			return "retain_sdk"
		}
		return "retain_sdk"
	case usesBlender:
		return "retain_blender_pack"
	case strings.HasPrefix(path, "docker/vllm/"):
		return "archive_research"
	// Go-bound operational / evidence authority (scripts/*.py, not spec-lab):
	case strings.HasPrefix(path, "scripts/") && !strings.HasPrefix(path, "scripts/spec-lab/"):
		if strings.HasPrefix(base, "test_") {
			return "rewrite_go" // its assertions migrate into Go tests
		}
		if containsAny(base, "source_fingerprint", "five-by-five", "runtime_matrix", "api_contract",
			"release_surface", "performance_proof", "fleet_proof", "validate_claims",
			"verify_proof_ledger", "cost_calculator", "supplier_earnings", "doc-as-test", "cx_status", "cx_trace") {
			return "rewrite_go"
		}
		if containsAny(base, "cx_logo", "cx_knob", "cx_button", "cx_marker") {
			return "archive_research" // asset generators -> render/docs pack
		}
		return "rewrite_go"
	// spec-lab: the Rust-bound speculative core + adapters, vs render research
	case strings.HasPrefix(path, "scripts/spec-lab/"):
		if containsAny(base, "speculative_core", "spec_adapter", "spec_receipts", "native_speculation", "integrated_speculation", "transcode_spec", "render_spec") {
			return "rewrite_rust"
		}
		if strings.HasPrefix(base, "test_") {
			return "rewrite_rust"
		}
		return "archive_research"
	case strings.HasPrefix(path, "render/"):
		if usesBlender {
			return "retain_blender_pack"
		}
		return "archive_research"
	default:
		return "archive_research"
	}
}

// ---- LOC counting ----

func countLOC(data []byte, lang string) (total, code, blank, comment int) {
	if len(data) == 0 {
		return 0, 0, 0, 0
	}
	lineComment, blockOpen, blockClose := commentTokens(lang)
	inBlock := false
	for _, raw := range strings.Split(string(data), "\n") {
		total++
		line := strings.TrimSpace(raw)
		if line == "" {
			blank++
			continue
		}
		if inBlock {
			comment++
			if blockClose != "" && strings.Contains(line, blockClose) {
				inBlock = false
			}
			continue
		}
		if blockOpen != "" && strings.HasPrefix(line, blockOpen) {
			comment++
			if blockClose == "" || !strings.Contains(line[len(blockOpen):], blockClose) {
				inBlock = true
			}
			continue
		}
		if lineComment != "" && strings.HasPrefix(line, lineComment) {
			comment++
			continue
		}
		code++
	}
	// a trailing newline yields a final empty split element; do not overcount
	if strings.HasSuffix(string(data), "\n") {
		total--
		blank--
	}
	return total, code, blank, comment
}

func commentTokens(lang string) (line, blockOpen, blockClose string) {
	switch lang {
	case "go", "rust", "javascript", "typescript", "swift", "shader", "proto", "web":
		return "//", "/*", "*/"
	case "python", "shell", "yaml", "toml", "config":
		return "#", "", ""
	case "sql":
		return "--", "/*", "*/"
	default:
		return "", "", ""
	}
}

// ---- binary detection ----

func looksBinary(path string, data []byte) bool {
	switch extLang[strings.ToLower(filepath.Ext(path))] {
	case "binary":
		return true
	}
	if len(data) == 0 {
		return false
	}
	sample := data
	if len(sample) > 8000 {
		sample = sample[:8000]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	return !utf8.Valid(sample)
}

// ---- helpers ----

func firstElem(path string) string {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// ---- artifact writers ----

func writeCensusArtifact(root, dir, name string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fatalf("marshal %s: %v", name, err)
	}
	b = append(b, '\n')
	p := filepath.Join(root, dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		fatalf("write %s: %v", p, err)
	}
}

type sumLOC struct {
	Files   int   `json:"files"`
	LOC     int   `json:"loc"`
	Code    int   `json:"code"`
	Blank   int   `json:"blank"`
	Comment int   `json:"comment"`
	Bytes   int64 `json:"bytes"`
}

func rollup(records []fileRecord, key func(fileRecord) string) map[string]*sumLOC {
	m := map[string]*sumLOC{}
	for _, r := range records {
		k := key(r)
		s := m[k]
		if s == nil {
			s = &sumLOC{}
			m[k] = s
		}
		s.Files++
		s.LOC += r.LOC
		s.Code += r.Code
		s.Blank += r.Blank
		s.Comment += r.Comment
		s.Bytes += r.Bytes
	}
	return m
}

func writeCensus(root, dir, head string, records []fileRecord) {
	byLayer := rollup(records, func(r fileRecord) string { return r.Layer })
	bySub := rollup(records, func(r fileRecord) string { return r.Subsystem })
	byLang := rollup(records, func(r fileRecord) string { return r.Language })

	var totalLOC, totalCode int
	var totalBytes int64
	for _, r := range records {
		totalLOC += r.LOC
		totalCode += r.Code
		totalBytes += r.Bytes
	}
	census := map[string]any{
		"head":          head,
		"tracked_files": len(records),
		"total_loc":     totalLOC,
		"total_code":    totalCode,
		"total_bytes":   totalBytes,
		"by_layer":      byLayer,
		"by_subsystem":  bySub,
		"by_language":   byLang,
		"files":         records,
	}
	writeCensusArtifact(root, dir, "CODEBASE_CENSUS.json", census)

	// markdown summary
	var b strings.Builder
	fmt.Fprintf(&b, "# ComputExchange Codebase Census\n\n")
	fmt.Fprintf(&b, "HEAD `%s` · %d tracked files · %d LOC (text) · %.1f MB tracked\n\n",
		short(head), len(records), totalLOC, float64(totalBytes)/1e6)
	fmt.Fprintf(&b, "Generated by `cx audit codebase` (retires `make loc`). Deterministic for a fixed commit.\n\n")

	fmt.Fprintf(&b, "## LOC by layer\n\n| layer | files | loc | bytes |\n|---|--:|--:|--:|\n")
	for _, k := range sortedKeys(byLayer) {
		s := byLayer[k]
		fmt.Fprintf(&b, "| %s | %d | %d | %.1f MB |\n", k, s.Files, s.LOC, float64(s.Bytes)/1e6)
	}
	fmt.Fprintf(&b, "\n## LOC by subsystem\n\n| subsystem | files | loc |\n|---|--:|--:|\n")
	for _, k := range sortedKeys(bySub) {
		s := bySub[k]
		fmt.Fprintf(&b, "| %s | %d | %d |\n", k, s.Files, s.LOC)
	}
	fmt.Fprintf(&b, "\n## LOC by language\n\n| language | files | loc | code | comment |\n|---|--:|--:|--:|--:|\n")
	for _, k := range sortedKeys(byLang) {
		s := byLang[k]
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d |\n", k, s.Files, s.LOC, s.Code, s.Comment)
	}
	fmt.Fprintf(&b, "\n## Layer definitions\n\n")
	b.WriteString("- **kernel**: owned, non-test, non-vendor source in {control, agent, spec-engine, token-spec-poc, cli, proto, db} in code languages (go/rust/sql/proto/shader).\n")
	b.WriteString("- **active_product**: kernel + those dirs' tests + {sdk, web(non-vendor), macapp}.\n")
	b.WriteString("- **active_repo_support**: scripts/ops/monitoring/config that ship in the default checkout but are not the product itself.\n")
	b.WriteString("- **historical**: docs (candidate for docs-archive pack).\n")
	b.WriteString("- **pack:NAME**: relocatable into an optional immutable pack (render-lab, benchmarks, docs-archive, proof-archive).\n")
	b.WriteString("- **vendored**: under a vendor/ path (upstream, accounted separately).\n")
	b.WriteString("- **generated**: deterministically regenerable / machine-emitted.\n")
	if err := os.WriteFile(filepath.Join(root, dir, "CODEBASE_CENSUS.md"), []byte(b.String()), 0o644); err != nil {
		fatalf("write census md: %v", err)
	}
}

func writeLOC(root, dir string, records []fileRecord) {
	bySub := rollup(records, func(r fileRecord) string { return r.Subsystem })
	byLang := rollup(records, func(r fileRecord) string { return r.Language })

	loc := func(m map[string]*sumLOC, k string) int {
		if s := m[k]; s != nil {
			return s.LOC
		}
		return 0
	}
	// derived owned totals — all layer-based so the buckets are disjoint and sum cleanly.
	var vendored, generated, kernel, test, historical, activeProductLayer, packLOC, totalLOC int
	for _, r := range records {
		totalLOC += r.LOC
		switch {
		case r.Vendored:
			vendored += r.LOC
		case r.Generated:
			generated += r.LOC
		}
		switch {
		case r.Layer == "kernel":
			kernel += r.LOC
		case r.Layer == "active_product":
			activeProductLayer += r.LOC
		case r.Layer == "historical":
			historical += r.LOC
		}
		if strings.HasPrefix(r.Layer, "pack:") {
			packLOC += r.LOC
			historical += r.LOC
		}
		if r.Classification == "test" {
			test += r.LOC
		}
	}
	out := map[string]any{
		"total_loc":             totalLOC,
		"kernel_LOC":            kernel,
		"active_product_LOC":    kernel + activeProductLayer, // kernel + {sdk, web, macapp, in-tree tests}
		"active_repository_LOC": totalLOC - vendored - generated - packLOC,
		"hydrated_owned_LOC":    totalLOC - vendored,
		"historical_LOC":        historical,
		"generated_LOC":         generated,
		"vendored_LOC":          vendored,
		"test_LOC":              test,
		"go_LOC":                loc(byLang, "go"),
		"rust_LOC":              loc(byLang, "rust"),
		"python_LOC":            loc(byLang, "python"),
		"swift_LOC":             loc(byLang, "swift"),
		"javascript_LOC":        loc(byLang, "javascript"),
		"shell_LOC":             loc(byLang, "shell"),
		"sql_LOC":               loc(byLang, "sql"),
		"shader_LOC":            loc(byLang, "shader"),
		"documentation_LOC":     loc(byLang, "documentation"),
		"control_LOC":           loc(bySub, "control"),
		"agent_LOC":             loc(bySub, "agent"),
		"speculation_LOC":       loc(bySub, "speculation"),
		"proof_LOC":             loc(bySub, "proof"),
		"payment_LOC":           loc(bySub, "payment"),
		"verification_LOC":      loc(bySub, "verification"),
		"interface_LOC":         loc(bySub, "interface"),
		"render_LOC":            loc(bySub, "render"),
		"sdk_LOC":               loc(bySub, "sdk"),
		"note":                  "kernel/active_* are derived per documented layer rules in CODEBASE_CENSUS.md; retires `make loc`.",
	}
	writeCensusArtifact(root, dir, "CODEBASE_LOC.json", out)
}

func writeBytes(root, dir string, records []fileRecord) {
	var total int64
	byLang := map[string]int64{}
	for _, r := range records {
		total += r.Bytes
		byLang[r.Language] += r.Bytes
	}
	type fb struct {
		Path  string `json:"path"`
		Bytes int64  `json:"bytes"`
	}
	sorted := make([]fileRecord, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Bytes > sorted[j].Bytes })
	top := []fb{}
	for i := 0; i < len(sorted) && i < 40; i++ {
		top = append(top, fb{sorted[i].Path, sorted[i].Bytes})
	}
	out := map[string]any{
		"tracked_bytes":      total,
		"bytes_by_language":  byLang,
		"largest_tracked_40": top,
		"note":               "tracked-only (git ls-files). Excludes .git, model weights, dep caches, untracked artifacts.",
	}
	writeCensusArtifact(root, dir, "CODEBASE_BYTES.json", out)
}

func writePython(root, dir string, records []fileRecord) {
	byPlan := map[string]*sumLOC{}
	files := map[string][]string{}
	unknown := 0
	for _, r := range records {
		if r.Language != "python" {
			continue
		}
		plan := r.PythonPlan
		if plan == "" || plan == "unknown" {
			unknown++
			plan = "unknown"
		}
		s := byPlan[plan]
		if s == nil {
			s = &sumLOC{}
			byPlan[plan] = s
		}
		s.Files++
		s.LOC += r.LOC
		s.Bytes += r.Bytes
		files[plan] = append(files[plan], r.Path)
	}
	out := map[string]any{
		"policy":            "production_python=0, proof_authority_python=0, speculative_runtime_python=0. Only retain_sdk + retain_blender_pack remain in a shipped surface.",
		"by_plan":           byPlan,
		"files_by_plan":     files,
		"unknown_remaining": unknown,
	}
	writeCensusArtifact(root, dir, "PYTHON_RECLAMATION.json", out)
}

func writeDeps(root, dir string) {
	out := map[string]any{
		"go_modules":   goModules(root),
		"cargo_crates": cargoCrates(root),
		"python":       map[string]any{"sdk": "dependency-free (stdlib only)", "note": "see sdk/python + docker/vllm"},
		"note":         "direct dependencies parsed from manifests; transitive counts from lockfiles.",
	}
	writeCensusArtifact(root, dir, "DEPENDENCY_CENSUS.json", out)
}

func goModules(root string) []map[string]any {
	mods := []string{"control/go.mod"}
	res := []map[string]any{}
	for _, m := range mods {
		data := readFileMaybe(filepath.Join(root, m))
		if data == nil {
			continue
		}
		direct := 0
		inReq := false
		for _, line := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "require (") {
				inReq = true
				continue
			}
			if inReq && t == ")" {
				inReq = false
				continue
			}
			if inReq && t != "" && !strings.Contains(t, "// indirect") {
				direct++
			} else if strings.HasPrefix(t, "require ") && !strings.Contains(t, "// indirect") {
				direct++
			}
		}
		res = append(res, map[string]any{"module": m, "direct_requires": direct})
	}
	return res
}

func cargoCrates(root string) []map[string]any {
	crates := []string{"agent/Cargo.toml", "spec-engine/Cargo.toml", "token-spec-poc/Cargo.toml", "renderer/Cargo.toml"}
	res := []map[string]any{}
	for _, c := range crates {
		data := readFileMaybe(filepath.Join(root, c))
		if data == nil {
			continue
		}
		direct := 0
		inDeps := false
		for _, line := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "[dependencies]") || strings.HasPrefix(t, "[dev-dependencies]") || strings.HasPrefix(t, "[build-dependencies]") {
				inDeps = true
				continue
			}
			if strings.HasPrefix(t, "[") {
				inDeps = false
				continue
			}
			if inDeps && t != "" && !strings.HasPrefix(t, "#") && strings.Contains(t, "=") {
				direct++
			}
		}
		res = append(res, map[string]any{"crate": c, "direct_deps": direct})
	}
	return res
}

func writeCandidates(root, dir string, records []fileRecord) {
	type cand struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
		Layer  string `json:"layer"`
		Bytes  int64  `json:"bytes"`
		LOC    int    `json:"loc"`
		Risk   string `json:"deletion_risk"`
	}
	var packBytes int64
	packByName := map[string]*sumLOC{}
	cands := []cand{}
	for _, r := range records {
		if r.PackEligible != "" {
			packBytes += r.Bytes
			s := packByName[r.PackEligible]
			if s == nil {
				s = &sumLOC{}
				packByName[r.PackEligible] = s
			}
			s.Files++
			s.LOC += r.LOC
			s.Bytes += r.Bytes
		}
		reason := ""
		switch {
		case r.PackEligible != "":
			reason = "relocate -> " + r.PackEligible
		case r.Generated:
			reason = "generated: prefer regenerate over track"
		case r.Vendored:
			reason = "vendored: account separately from owned"
		case r.PythonPlan == "rewrite_go":
			reason = "python -> Go (operational/evidence authority)"
		case r.PythonPlan == "rewrite_rust":
			reason = "python -> Rust (speculative core)"
		}
		if reason != "" {
			cands = append(cands, cand{r.Path, reason, r.Layer, r.Bytes, r.LOC, r.DeletionRisk})
		}
	}
	out := map[string]any{
		"pack_rollup":      packByName,
		"pack_bytes_total": packBytes,
		"candidate_count":  len(cands),
		"candidates":       cands,
	}
	writeCensusArtifact(root, dir, "CONDENSATION_CANDIDATES.json", out)
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
