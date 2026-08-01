package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	projectIRVersion        = 1
	projectMaxFiles         = 10_000
	projectMaxInspectBytes  = 16 << 20
	projectMaxFileBytes     = 256 << 10
	projectProbeMaxBytes    = 1 << 20
	projectMaxArtifactBytes = 4 << 30
	projectMaxTotalBytes    = 64 << 30
)

// ProjectWorkloadIR is a proposal, not an executable JobManifest. It preserves
// the graph and every uncertainty that must be resolved before pricing or
// admission. A caller cannot turn detector confidence into a sellable quote.
type ProjectWorkloadIR struct {
	Version        int                  `json:"version"`
	ProjectSHA256  string               `json:"project_sha256"`
	IRSHA256       string               `json:"ir_sha256"`
	Status         string               `json:"status"`
	Steps          []ProjectIRStep      `json:"steps"`
	Artifacts      []ProjectIRArtifact  `json:"artifacts"`
	Privacy        ProjectIRPrivacy     `json:"privacy"`
	Quality        ProjectIRQuality     `json:"quality"`
	Deadline       *ProjectIRDeadline   `json:"deadline,omitempty"`
	Result         ProjectIRResult      `json:"result"`
	Economics      ProjectIREconomics   `json:"economics"`
	Estimate       ProjectIREstimate    `json:"estimate"`
	Probe          ProjectIRProbe       `json:"probe"`
	Detections     []ProjectIRDetection `json:"detections"`
	Unknowns       []string             `json:"unknowns"`
	RefusalReasons []string             `json:"refusal_reasons,omitempty"`
}

type ProjectIRStep struct {
	ID               string   `json:"id"`
	Kind             string   `json:"kind"`
	DependsOn        []string `json:"depends_on"`
	Inputs           []string `json:"inputs"`
	Outputs          []string `json:"outputs"`
	RuntimeContract  string   `json:"runtime_contract"`
	ModelContract    string   `json:"model_contract,omitempty"`
	ResourceEstimate string   `json:"resource_estimate"`
	Parallelism      string   `json:"parallelism"`
	CheckpointPolicy string   `json:"checkpoint_policy"`
	Verification     string   `json:"verification"`
}

type ProjectIRArtifact struct {
	Path      string `json:"path"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
}

type ProjectIRPrivacy struct {
	Egress       string `json:"egress"`
	DataLocation string `json:"data_location"`
}

type ProjectIRQuality struct {
	Requirement  string `json:"requirement"`
	Verification string `json:"verification"`
}

type ProjectIRDeadline struct {
	RFC3339 string `json:"rfc3339"`
}

type ProjectIRResult struct {
	Contract  string `json:"contract"`
	Retention string `json:"retention"`
	Delivery  string `json:"delivery"`
}

type ProjectIREconomics struct {
	Currency              string `json:"currency"`
	MaximumBuyerPrice     string `json:"maximum_buyer_price"`
	SupplierFloor         string `json:"supplier_floor"`
	MercContribution      string `json:"merc_contribution"`
	PricingDecisionSHA256 string `json:"pricing_decision_sha256,omitempty"`
}

type ProjectIREstimate struct {
	State              string  `json:"state"`
	CostConfidence     float64 `json:"cost_confidence"`
	DurationConfidence float64 `json:"duration_confidence"`
	CalibrationBasis   string  `json:"calibration_basis"`
}

type ProjectIRProbe struct {
	Requested        bool     `json:"requested"`
	BuyerAuthorized  bool     `json:"buyer_authorized"`
	ApprovedIRSHA256 string   `json:"approved_ir_sha256,omitempty"`
	Executed         bool     `json:"executed"`
	Kind             string   `json:"kind,omitempty"`
	BytesRead        int64    `json:"bytes_read"`
	Observations     []string `json:"observations,omitempty"`
}

type ProjectIRDetection struct {
	Kind       string   `json:"kind"`
	Confidence float64  `json:"confidence"`
	Evidence   []string `json:"evidence"`
}

type projectCompileOptions struct {
	Root                  string
	ProbeRequested        bool
	BuyerApprovedIRSHA256 string
}

type projectFile struct {
	rel     string
	size    int64
	digest  string
	content []byte
}

var ignoredProjectDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, "node_modules": true,
	"vendor": true, "target": true, "dist": true, "build": true,
}

var sensitiveProjectNames = map[string]bool{
	".env": true, ".npmrc": true, ".pypirc": true, "credentials": true,
	"credentials.json": true, "id_rsa": true, "id_ed25519": true,
}

func compileProject(opts projectCompileOptions) (ProjectWorkloadIR, error) {
	if !opts.ProbeRequested && opts.BuyerApprovedIRSHA256 != "" {
		return ProjectWorkloadIR{}, errors.New("buyer-approved IR digest supplied without a probe request")
	}
	if opts.ProbeRequested && !validSHA256(opts.BuyerApprovedIRSHA256) {
		return ProjectWorkloadIR{}, errors.New("bounded probe requires the buyer-approved unprobed IR SHA-256")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return ProjectWorkloadIR{}, fmt.Errorf("resolve project root: %w", err)
	}
	files, err := inspectProject(root)
	if err != nil {
		return ProjectWorkloadIR{}, err
	}
	if opts.ProbeRequested {
		proposal := buildProjectIR(files, projectCompileOptions{Root: opts.Root})
		proposalDigest, digestErr := projectIRDigest(proposal)
		if digestErr != nil {
			return ProjectWorkloadIR{}, digestErr
		}
		if proposalDigest != opts.BuyerApprovedIRSHA256 {
			return ProjectWorkloadIR{}, fmt.Errorf(
				"buyer-approved IR digest %s does not match current proposal %s; refusing changed project",
				opts.BuyerApprovedIRSHA256, proposalDigest)
		}
	}
	ir := buildProjectIR(files, opts)
	digest, err := projectIRDigest(ir)
	if err != nil {
		return ProjectWorkloadIR{}, err
	}
	ir.IRSHA256 = digest
	return ir, nil
}

func inspectProject(root string) ([]projectFile, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect project root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("project root must be a directory, not a symlink")
	}
	var files []projectFile
	var inspected int64
	var totalBytes int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		base := strings.ToLower(entry.Name())
		if entry.IsDir() && ignoredProjectDirs[base] {
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("project contains symlink %q; refusing ambiguous project boundary", filepath.ToSlash(rel))
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("project contains non-regular file %q", filepath.ToSlash(rel))
		}
		if sensitiveProjectNames[base] || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") {
			return fmt.Errorf("project contains sensitive path %q; provide a redacted inventory", filepath.ToSlash(rel))
		}
		if len(files) >= projectMaxFiles {
			return fmt.Errorf("project exceeds %d-file inspection ceiling", projectMaxFiles)
		}
		stat, err := entry.Info()
		if err != nil {
			return err
		}
		if stat.Size() > projectMaxArtifactBytes {
			return fmt.Errorf("file %q exceeds %d-byte artifact ceiling", filepath.ToSlash(rel), projectMaxArtifactBytes)
		}
		totalBytes += stat.Size()
		if totalBytes > projectMaxTotalBytes {
			return fmt.Errorf("project exceeds %d-byte artifact ceiling", projectMaxTotalBytes)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		contentLimit := int64(projectMaxFileBytes)
		if remaining := int64(projectMaxInspectBytes) - inspected; remaining < contentLimit {
			contentLimit = remaining
		}
		var content []byte
		if contentLimit > 0 {
			content, err = io.ReadAll(io.LimitReader(file, contentLimit))
			if err == nil {
				inspected += int64(len(content))
				_, err = hash.Write(content)
			}
		}
		if err == nil {
			_, err = io.Copy(hash, file)
		}
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		files = append(files, projectFile{rel: filepath.ToSlash(rel), size: stat.Size(), digest: hex.EncodeToString(hash.Sum(nil)), content: content})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("static project inspection: %w", err)
	}
	if len(files) == 0 {
		return nil, errors.New("project contains no inspectable files")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

func buildProjectIR(files []projectFile, opts projectCompileOptions) ProjectWorkloadIR {
	ir := ProjectWorkloadIR{
		Version: projectIRVersion, Status: "PROPOSED_NOT_ADMISSIBLE",
		Privacy: ProjectIRPrivacy{Egress: "UNSPECIFIED_REFUSE", DataLocation: "UNSPECIFIED_REFUSE"},
		Quality: ProjectIRQuality{Requirement: "UNSPECIFIED_REFUSE", Verification: "UNSPECIFIED_REFUSE"},
		Result: ProjectIRResult{
			Contract: "UNRESOLVED_REFUSE", Retention: "UNRESOLVED_REFUSE", Delivery: "UNRESOLVED_REFUSE",
		},
		Economics: ProjectIREconomics{
			Currency: "UNRESOLVED_REFUSE", MaximumBuyerPrice: "UNRESOLVED_REFUSE",
			SupplierFloor: "UNRESOLVED_REFUSE", MercContribution: "UNRESOLVED_REFUSE",
		},
		Estimate: ProjectIREstimate{State: "UNCALIBRATED_REFUSE", CalibrationBasis: "no outcome-linked calibration cohort"},
		Probe: ProjectIRProbe{
			Requested: opts.ProbeRequested, BuyerAuthorized: opts.ProbeRequested,
			ApprovedIRSHA256: opts.BuyerApprovedIRSHA256,
		},
		Unknowns: []string{"buyer deadline", "data residency and egress permission", "quality threshold", "runtime/model revisions", "outcome-linked cost and duration calibration"},
	}
	projectHash := sha256.New()
	for _, file := range files {
		fmt.Fprintf(projectHash, "%s\x00%d\x00%s\n", file.rel, file.size, file.digest)
		ir.Artifacts = append(ir.Artifacts, ProjectIRArtifact{Path: file.rel, Bytes: file.size, SHA256: file.digest, MediaType: projectMediaType(file.rel)})
	}
	ir.ProjectSHA256 = hex.EncodeToString(projectHash.Sum(nil))
	ir.Detections = detectProjectWorkloads(files)
	for _, reason := range unsafeProjectSignals(files) {
		ir.RefusalReasons = append(ir.RefusalReasons, reason)
	}
	for i, detection := range ir.Detections {
		stepID := fmt.Sprintf("step-%02d-%s", i+1, detection.Kind)
		depends := []string(nil)
		if i > 0 {
			depends = []string{ir.Steps[i-1].ID}
		}
		ir.Steps = append(ir.Steps, ProjectIRStep{
			ID: stepID, Kind: detection.Kind, DependsOn: depends,
			Inputs: []string{"project://input"}, Outputs: []string{"project://" + stepID},
			RuntimeContract: "UNRESOLVED_REFUSE", ModelContract: "UNRESOLVED_REFUSE",
			ResourceEstimate: "UNCALIBRATED_REFUSE", Parallelism: "UNRESOLVED_REFUSE",
			CheckpointPolicy: "REQUIRED_UNRESOLVED", Verification: "REQUIRED_UNRESOLVED",
		})
	}
	if opts.ProbeRequested {
		runBoundedProjectProbe(files, &ir.Probe)
	}
	if len(ir.Detections) == 0 {
		ir.RefusalReasons = append(ir.RefusalReasons, "no supported workload detector reached confidence floor")
	}
	for _, d := range ir.Detections {
		if d.Confidence < 0.70 {
			ir.RefusalReasons = append(ir.RefusalReasons, "low-confidence detector: "+d.Kind)
		}
	}
	ir.RefusalReasons = append(ir.RefusalReasons, "cost and duration estimates are not calibrated against outcome-linked observations")
	sort.Strings(ir.RefusalReasons)
	return ir
}

func detectProjectWorkloads(files []projectFile) []ProjectIRDetection {
	evidence := map[string][]string{}
	add := func(kind, path string) { evidence[kind] = append(evidence[kind], path) }
	for _, file := range files {
		lower := strings.ToLower(file.rel)
		body := strings.ToLower(string(file.content))
		switch {
		case strings.HasSuffix(lower, "dockerfile") || strings.HasSuffix(lower, "compose.yml") || strings.HasSuffix(lower, "compose.yaml"):
			add("bounded_container", file.rel)
		case strings.HasSuffix(lower, ".ipynb"):
			add("batch_compute", file.rel)
		case strings.HasSuffix(lower, ".safetensors") || strings.Contains(lower, "lora"):
			add("lora_training", file.rel)
		case strings.HasSuffix(lower, ".png") || strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg") || strings.HasSuffix(lower, ".mp4"):
			add("media_rendering", file.rel)
		}
		for _, signal := range []struct{ kind, token string }{
			{"realtime_inference", "websocket"}, {"embeddings", "embedding"},
			{"structured_extraction", "json_schema"}, {"model_evaluation", "eval"},
			{"service_deployment", "deployment"}, {"image_video", "diffusers"},
			{"batch_inference", "batch_infer"},
		} {
			if strings.Contains(body, signal.token) {
				add(signal.kind, file.rel)
			}
		}
	}
	kinds := make([]string, 0, len(evidence))
	for kind := range evidence {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	out := make([]ProjectIRDetection, 0, len(kinds))
	for _, kind := range kinds {
		paths := normalizeUniqueStrings(evidence[kind], strings.TrimSpace)
		confidence := 0.55
		if len(paths) >= 2 {
			confidence = 0.72
		}
		out = append(out, ProjectIRDetection{Kind: kind, Confidence: confidence, Evidence: paths})
	}
	return out
}

func unsafeProjectSignals(files []projectFile) []string {
	var reasons []string
	for _, file := range files {
		lower := strings.ToLower(file.rel)
		body := strings.ToLower(string(file.content))
		if (strings.HasSuffix(lower, "compose.yml") || strings.HasSuffix(lower, "compose.yaml")) &&
			(strings.Contains(body, "privileged: true") || strings.Contains(body, "network_mode: host")) {
			reasons = append(reasons, "unsafe container authority in "+file.rel)
		}
		if strings.HasSuffix(lower, "dockerfile") &&
			(strings.Contains(body, "--privileged") || strings.Contains(body, "/var/run/docker.sock")) {
			reasons = append(reasons, "unsafe container authority in "+file.rel)
		}
	}
	return normalizeUniqueStrings(reasons, strings.TrimSpace)
}

func runBoundedProjectProbe(files []projectFile, probe *ProjectIRProbe) {
	probe.Executed = true
	probe.Kind = "NON_EXECUTING_FILE_SHAPE_V1"
	var remaining int64 = projectProbeMaxBytes
	for _, file := range files {
		if remaining <= 0 {
			break
		}
		read := int64(len(file.content))
		if read > remaining {
			read = remaining
		}
		probe.BytesRead += read
		remaining -= read
		lower := strings.ToLower(file.rel)
		if strings.HasSuffix(lower, ".jsonl") || strings.HasSuffix(lower, ".ndjson") {
			scanner := bufio.NewScanner(strings.NewReader(string(file.content[:read])))
			records := 0
			for scanner.Scan() && records < 256 {
				records++
			}
			probe.Observations = append(probe.Observations, fmt.Sprintf("%s:sample_records=%d", file.rel, records))
		}
	}
	probe.Observations = append(probe.Observations, fmt.Sprintf("inventory_files=%d", len(files)))
	sort.Strings(probe.Observations)
}

func projectMediaType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return "application/json"
	case ".jsonl", ".ndjson":
		return "application/x-ndjson"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".mp4":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}

func projectIRDigest(ir ProjectWorkloadIR) (string, error) {
	ir.IRSHA256 = ""
	blob, err := json.Marshal(ir)
	if err != nil {
		return "", fmt.Errorf("marshal project IR: %w", err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

func writeProjectIR(w io.Writer, ir ProjectWorkloadIR) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(ir)
}
