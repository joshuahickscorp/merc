package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

type fileRecord struct {
	Path      string `json:"path"`
	Language  string `json:"language"`
	Subsystem string `json:"subsystem"`
	Kind      string `json:"kind"`
	LOC       int    `json:"loc"`
	Code      int    `json:"code"`
	Blank     int    `json:"blank"`
	Comment   int    `json:"comment"`
	Bytes     int64  `json:"bytes"`
	Binary    bool   `json:"-"`
	Generated bool   `json:"-"`
	Vendored  bool   `json:"-"`
}

type sumLOC struct {
	Files   int   `json:"files"`
	LOC     int   `json:"loc"`
	Code    int   `json:"code"`
	Blank   int   `json:"blank"`
	Comment int   `json:"comment"`
	Bytes   int64 `json:"bytes"`
}

type dependencyCensus struct {
	GoDirect     int `json:"go_direct"`
	CargoDirect  int `json:"cargo_direct"`
	PythonDirect int `json:"python_direct"`
	TotalDirect  int `json:"total_direct"`
}

type ownershipTotals struct {
	GlobalOwnedLOC   int `json:"global_owned_loc"`
	TestLOC          int `json:"test_loc"`
	DocumentationLOC int `json:"documentation_loc"`
	PythonLOC        int `json:"python_loc"`
	GeneratedLOC     int `json:"generated_loc"`
	VendoredLOC      int `json:"vendored_upstream_loc"`
	RelocatedLOC     int `json:"relocated_loc"`
	PatchLOC         int `json:"first_party_patch_loc"`
}

var extLang = map[string]string{
	".go": "go", ".rs": "rust", ".py": "python", ".swift": "swift",
	".js": "javascript", ".mjs": "javascript", ".ts": "typescript",
	".sql": "sql", ".metal": "shader", ".sh": "shell", ".bash": "shell",
	".md": "documentation", ".json": "json", ".jsonl": "json",
	".yml": "yaml", ".yaml": "yaml", ".toml": "toml", ".proto": "proto",
	".html": "web", ".css": "web", ".svg": "web", ".webmanifest": "web",
	".xml": "config", ".plist": "config", ".sb": "config", ".txt": "text",
	".png": "binary", ".jpg": "binary", ".jpeg": "binary", ".webp": "binary",
	".pdf": "binary", ".glb": "binary", ".gltf": "binary", ".onnx": "binary",
	".pt": "binary", ".woff2": "binary", ".ttf": "binary", ".ico": "binary",
	".br": "binary", ".mp4": "binary", ".wasm": "binary", ".gz": "binary",
	".lock": "lockfile", ".sum": "lockfile", ".resolved": "lockfile",
	".mod": "config", ".dockerignore": "config", ".gitignore": "config",
}

func cmdAudit(args []string) {
	if len(args) == 0 || args[0] != "codebase" {
		fatalf("usage: cx audit codebase [--out DIR]")
	}
	out := "census"
	for i := 1; i < len(args); i++ {
		if args[i] != "--out" {
			fatalf("unknown flag %q", args[i])
		}
		out = next(args, &i)
	}
	root := repoRoot()
	if err := os.MkdirAll(filepath.Join(root, out), 0o755); err != nil {
		fatalf("mkdir %s: %v", out, err)
	}
	for range 2 {
		writeAudit(root, out, scanTracked(root))
	}
	records := scanTracked(root)
	totals := auditTotals(records)
	fmt.Printf("cx audit codebase: GLOBAL_OWNED_LOC=%d files=%d bytes=%d\n",
		totals.GlobalOwnedLOC, len(records), sumBytes(records))
	fmt.Printf("ledger: %s\nsummary: %s\n",
		filepath.Join(out, "CODEBASE_CENSUS.json"), filepath.Join(out, "CODEBASE_CENSUS.md"))
}

func repoRoot() string {
	o, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fatalf("not a git repo: %v", err)
	}
	return strings.TrimSpace(string(o))
}

func gitTracked(root string) []string {
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	o, err := cmd.Output()
	if err != nil {
		fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, p := range bytes.Split(o, []byte{0}) {
		if len(p) > 0 {
			files = append(files, string(p))
		}
	}
	return files
}

func scanTracked(root string) []fileRecord {
	var records []fileRecord
	for _, rel := range gitTracked(root) {
		abs := filepath.Join(root, rel)
		info, err := os.Stat(abs)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		r := fileRecord{Path: rel, Language: languageOf(rel), Subsystem: subsystemOf(rel), Bytes: info.Size()}
		r.Vendored = strings.Contains(rel, "/vendor/") || strings.HasPrefix(rel, "vendor/")
		r.Binary = looksBinary(rel, data)
		r.Generated = isGenerated(rel, data)
		if !r.Binary {
			r.LOC, r.Code, r.Blank, r.Comment = countLOC(data, r.Language)
		}
		r.Kind = kindOf(r)
		records = append(records, r)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	return records
}

func languageOf(path string) string {
	if lang := extLang[strings.ToLower(filepath.Ext(path))]; lang != "" {
		return lang
	}
	switch filepath.Base(path) {
	case "Dockerfile", "Makefile", "Caddyfile", "go.mod", "Cargo.toml":
		return "config"
	case "cx":
		return "shell"
	default:
		return "other"
	}
}

func subsystemOf(path string) string {
	top, base := firstElem(path), filepath.Base(path)
	if strings.Contains(path, "/vendor/") || strings.HasPrefix(path, "vendor/") {
		return "vendored"
	}
	switch top {
	case "agent":
		return "agent"
	case "control":
		switch {
		case base == "schema.sql":
			return "store"
		case containsAny(base, "audit", "prove", "evidence", "buildinfo"):
			return "proof"
		case containsAny(base, "billing", "payout", "ledger", "stripe", "payment", "invoice", "refund", "dispute", "charge", "economic", "collect", "reconcile"):
			return "payment"
		case containsAny(base, "verif", "reputation", "honeypot", "fraud", "redundan", "result_validation"):
			return "verification"
		default:
			return "control"
		}
	case "proto":
		return "contract"
	case "sdk":
		return "sdk"
	case "web", "macapp", "logo":
		return "interface"
	case "docs":
		return "documentation"
	case "scripts", "proof":
		return "proof"
	default:
		return "ops"
	}
}

func kindOf(r fileRecord) string {
	base := filepath.Base(r.Path)
	switch {
	case r.Vendored:
		return "vendored"
	case r.Binary:
		return "binary"
	case r.Generated:
		return "generated"
	case strings.HasSuffix(base, "_test.go"), strings.HasPrefix(base, "test_"), strings.Contains(r.Path, "/tests/"):
		return "test"
	case r.Language == "documentation":
		return "documentation"
	default:
		return "maintained"
	}
}

func isGenerated(path string, data []byte) bool {
	if filepath.Base(path) == "Cargo.lock" || strings.Contains(filepath.Base(path), ".generated.") {
		return true
	}
	head := data
	if len(head) > 400 {
		head = head[:400]
	}
	h := strings.ToLower(string(head))
	return strings.Contains(h, "code generated") || strings.Contains(h, "do not edit") || strings.Contains(h, "@generated")
}

func countLOC(data []byte, lang string) (total, code, blank, comment int) {
	if len(data) == 0 {
		return
	}
	lineComment, blockOpen, blockClose := commentTokens(lang)
	inBlock := false
	for _, raw := range strings.Split(string(data), "\n") {
		total++
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			blank++
		case inBlock:
			comment++
			if blockClose != "" && strings.Contains(line, blockClose) {
				inBlock = false
			}
		case blockOpen != "" && strings.HasPrefix(line, blockOpen):
			comment++
			inBlock = blockClose == "" || !strings.Contains(line[len(blockOpen):], blockClose)
		case lineComment != "" && strings.HasPrefix(line, lineComment):
			comment++
		default:
			code++
		}
	}
	if data[len(data)-1] == '\n' {
		total--
		blank--
	}
	return
}

func commentTokens(lang string) (line, open, close string) {
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

func looksBinary(path string, data []byte) bool {
	if extLang[strings.ToLower(filepath.Ext(path))] == "binary" {
		return true
	}
	return bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
}

func rollup(records []fileRecord, key func(fileRecord) string) map[string]*sumLOC {
	out := map[string]*sumLOC{}
	for _, r := range records {
		if r.Vendored {
			continue
		}
		k := key(r)
		s := out[k]
		if s == nil {
			s = &sumLOC{}
			out[k] = s
		}
		s.Files++
		s.LOC += r.LOC
		s.Code += r.Code
		s.Blank += r.Blank
		s.Comment += r.Comment
		s.Bytes += r.Bytes
	}
	return out
}

func auditTotals(records []fileRecord) ownershipTotals {
	var t ownershipTotals
	for _, r := range records {
		if r.Vendored {
			t.VendoredLOC += r.LOC
			continue
		}
		t.GlobalOwnedLOC += r.LOC
		if r.Kind == "test" {
			t.TestLOC += r.LOC
		}
		if r.Language == "documentation" {
			t.DocumentationLOC += r.LOC
		}
		if r.Language == "python" {
			t.PythonLOC += r.LOC
		}
		if r.Generated {
			t.GeneratedLOC += r.LOC
		}
	}
	return t
}

func directDependencies(root string) dependencyCensus {
	d := dependencyCensus{GoDirect: goDirect(filepath.Join(root, "control", "go.mod")), CargoDirect: cargoDirect(filepath.Join(root, "agent", "Cargo.toml"))}
	d.TotalDirect = d.GoDirect + d.CargoDirect + d.PythonDirect
	return d
}

func goDirect(path string) int {
	b, _ := os.ReadFile(path)
	in, count := false, 0
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "require (" {
			in = true
			continue
		}
		if in && line == ")" {
			in = false
			continue
		}
		if in && line != "" && !strings.Contains(line, "// indirect") {
			count++
		} else if strings.HasPrefix(line, "require ") && !strings.Contains(line, "// indirect") {
			count++
		}
	}
	return count
}

func cargoDirect(path string) int {
	b, _ := os.ReadFile(path)
	in, count := false, 0
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "[dependencies]" || line == "[dev-dependencies]" || line == "[build-dependencies]" {
			in = true
			continue
		}
		if strings.HasPrefix(line, "[") {
			in = false
		} else if in && line != "" && !strings.HasPrefix(line, "#") && strings.Contains(line, "=") {
			count++
		}
	}
	return count
}

func sourceIdentity(root string, records []fileRecord) string {
	h := sha256.New()
	for _, r := range records {
		if r.Path == "census/CODEBASE_CENSUS.json" || r.Path == "census/CODEBASE_CENSUS.md" {
			continue
		}
		h.Write([]byte(r.Path))
		h.Write([]byte{0})
		b, _ := os.ReadFile(filepath.Join(root, r.Path))
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func routeCount(root string) int {
	return countNeedle(filepath.Join(root, "control", "api.go"), "mux.Handle")
}
func tableCount(root string) int {
	return countNeedle(filepath.Join(root, "control", "schema.sql"), "CREATE TABLE IF NOT EXISTS")
}

func countNeedle(path, needle string) int {
	b, _ := os.ReadFile(path)
	count := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, needle) {
			count++
		}
	}
	return count
}

func sumBytes(records []fileRecord) int64 {
	var n int64
	for _, r := range records {
		n += r.Bytes
	}
	return n
}

func writeAudit(root, dir string, records []fileRecord) {
	files := map[string]int{}
	for _, r := range records {
		if !r.Vendored {
			files[r.Path] = r.LOC
		}
	}
	ledger := map[string]any{
		"schema_version":     1,
		"authority_sha256":   sourceIdentity(root, records),
		"scope":              "tracked maintained first-party text; compiled binaries and upstream vendor excluded",
		"tracked_files":      len(records),
		"tracked_bytes":      sumBytes(records),
		"ownership":          auditTotals(records),
		"owned_by_language":  rollup(records, func(r fileRecord) string { return r.Language }),
		"owned_by_subsystem": rollup(records, func(r fileRecord) string { return r.Subsystem }),
		"dependencies":       directDependencies(root),
		"surface": map[string]any{
			"binaries": []string{"cx", "cx-agent"}, "routes": routeCount(root), "tables": tableCount(root),
			"workloads": []string{"embed", "batch_infer"}, "runtimes": []string{"candle_metal"},
		},
		"files": files,
	}
	b, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		fatalf("marshal census: %v", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(filepath.Join(root, dir, "CODEBASE_CENSUS.json"), b, 0o644); err != nil {
		fatalf("write census: %v", err)
	}

	totals := auditTotals(records)
	byLang := rollup(records, func(r fileRecord) string { return r.Language })
	bySub := rollup(records, func(r fileRecord) string { return r.Subsystem })
	var md strings.Builder
	fmt.Fprintf(&md, "# Codebase census\n\nGLOBAL_OWNED_LOC: **%d** across %d tracked files (%.2f MB).\n\n", totals.GlobalOwnedLOC, len(records), float64(sumBytes(records))/1e6)
	fmt.Fprintf(&md, "Tests: %d LOC · documentation: %d LOC · Python: %d LOC · generated: %d LOC · vendored upstream: %d LOC.\n\n", totals.TestLOC, totals.DocumentationLOC, totals.PythonLOC, totals.GeneratedLOC, totals.VendoredLOC)
	md.WriteString("## Owned LOC by language\n\n| language | files | loc |\n|---|--:|--:|\n")
	for _, k := range sortedKeys(byLang) {
		fmt.Fprintf(&md, "| %s | %d | %d |\n", k, byLang[k].Files, byLang[k].LOC)
	}
	md.WriteString("\n## Owned LOC by subsystem\n\n| subsystem | files | loc |\n|---|--:|--:|\n")
	for _, k := range sortedKeys(bySub) {
		fmt.Fprintf(&md, "| %s | %d | %d |\n", k, bySub[k].Files, bySub[k].LOC)
	}
	d := directDependencies(root)
	fmt.Fprintf(&md, "\nSurface: 2 binaries · 2 workloads · 1 runtime · %d routes · %d tables · %d direct dependencies.\n\n", routeCount(root), tableCount(root), d.TotalDirect)
	md.WriteString("`census/CODEBASE_CENSUS.json` is the sole machine-readable ownership ledger. This file is its concise human summary.\n")
	if err := os.WriteFile(filepath.Join(root, dir, "CODEBASE_CENSUS.md"), []byte(md.String()), 0o644); err != nil {
		fatalf("write census summary: %v", err)
	}
}

func firstElem(path string) string {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

func containsAny(s string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(s, value) {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
