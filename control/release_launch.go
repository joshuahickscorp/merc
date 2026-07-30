package main

// The Level-B launcher is intentionally a thin, fail-closed authority over the
// existing audited release adapters.  It never manufactures external evidence
// or handles a secret value beyond loading a mode-0600 operator file into its
// own process environment.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const levelBEnvironment = "staging"

type launchConfig struct {
	SchemaVersion int    `yaml:"schema_version" json:"schema_version"`
	Environment   string `yaml:"environment" json:"environment"`
	Candidate     struct {
		Commit       string `yaml:"commit" json:"commit"`
		ControlImage string `yaml:"control_image" json:"control_image"`
		PriorImage   string `yaml:"prior_image" json:"prior_image"`
		PriorCommit  string `yaml:"prior_commit" json:"prior_commit"`
	} `yaml:"candidate" json:"candidate"`
	Staging struct {
		SSHTarget       string `yaml:"ssh_target" json:"ssh_target"`
		TLSHostname     string `yaml:"tls_hostname" json:"tls_hostname"`
		DeploymentRoot  string `yaml:"deployment_root" json:"deployment_root"`
		StorageHostname string `yaml:"storage_tls_hostname" json:"storage_tls_hostname"`
		BindAddress     string `yaml:"bind_address" json:"bind_address"`
		ACMEEmail       string `yaml:"acme_email" json:"acme_email"`
	} `yaml:"staging" json:"staging"`
	Pricing struct {
		ReferenceToSettlementRate string `yaml:"reference_to_settlement_rate" json:"reference_to_settlement_rate"`
		FXRevision                string `yaml:"fx_revision" json:"fx_revision"`
	} `yaml:"pricing" json:"pricing"`
	Images struct {
		Prometheus   string `yaml:"prometheus" json:"prometheus"`
		Alertmanager string `yaml:"alertmanager" json:"alertmanager"`
		Grafana      string `yaml:"grafana" json:"grafana"`
		NodeExporter string `yaml:"node_exporter" json:"node_exporter"`
	} `yaml:"images" json:"images"`
	Backup struct {
		Offsite             string `yaml:"offsite" json:"offsite"`
		EncryptionRecipient string `yaml:"encryption_recipient" json:"encryption_recipient"`
	} `yaml:"backup" json:"backup"`
	Stripe struct {
		ConnectClientID          string `yaml:"connect_client_id" json:"connect_client_id"`
		TestConnectedAccountID   string `yaml:"test_connected_account_id" json:"test_connected_account_id"`
		BillingWebhookEndpointID string `yaml:"billing_webhook_endpoint_id" json:"billing_webhook_endpoint_id"`
		ConnectWebhookEndpointID string `yaml:"connect_webhook_endpoint_id" json:"connect_webhook_endpoint_id"`
	} `yaml:"stripe" json:"stripe"`
	Canary struct {
		ApprovedBuyerEmails   []string `yaml:"approved_buyer_emails" json:"approved_buyer_emails"`
		ApprovedWorkerIDs     []string `yaml:"approved_worker_ids" json:"approved_worker_ids"`
		ApprovedAgentVersions []string `yaml:"approved_agent_versions" json:"approved_agent_versions"`
		ApprovedBuildHashes   []string `yaml:"approved_build_hashes" json:"approved_build_hashes"`
		ScenarioDriver        string   `yaml:"scenario_driver" json:"scenario_driver"`
		ScenarioDriverSHA256  string   `yaml:"scenario_driver_sha256" json:"scenario_driver_sha256"`
		RestartDriver         string   `yaml:"restart_driver" json:"restart_driver"`
		RestartDriverSHA256   string   `yaml:"restart_driver_sha256" json:"restart_driver_sha256"`
	} `yaml:"canary" json:"canary"`
	Alert struct {
		ReceiverName string `yaml:"receiver_name" json:"receiver_name"`
	} `yaml:"alert" json:"alert"`
	Review struct {
		GitHubReviewerLogin string `yaml:"github_reviewer_login" json:"github_reviewer_login"`
	} `yaml:"review" json:"review"`
	Governance struct {
		ApprovalBundlePath string `yaml:"approval_bundle_path" json:"approval_bundle_path"`
	} `yaml:"governance" json:"governance"`
	Evidence struct {
		GovernanceReceipt string `yaml:"governance_receipt" json:"governance_receipt"`
	} `yaml:"evidence" json:"evidence"`
}

type launchPlan struct {
	SchemaVersion        int               `json:"schema_version"`
	Kind                 string            `json:"kind"`
	Environment          string            `json:"environment"`
	CandidateCommit      string            `json:"candidate_commit"`
	SourceSHA256         string            `json:"source_sha256"`
	ConfigSHA256         string            `json:"config_sha256"`
	SecretNamesSHA256    string            `json:"secret_names_sha256"`
	RemoteProfileSHA256  string            `json:"remote_profile_sha256"`
	IdentityFingerprints map[string]string `json:"identity_secret_fingerprints"`
	RequiredCommands     []string          `json:"required_commands"`
	LiveMoneyProhibited  bool              `json:"live_money_prohibited"`
	PlanSHA256           string            `json:"plan_sha256"`
}

type launchState struct {
	SchemaVersion  int          `json:"schema_version"`
	Status         string       `json:"status"`
	Plan           launchPlan   `json:"plan"`
	LastOperation  string       `json:"last_operation,omitempty"`
	AdapterHistory []adapterRun `json:"adapter_history,omitempty"`
}

// adapterRun records execution continuity without retaining command output.
// Adapter output can include operator-specific topology data and is therefore
// intentionally represented only by a digest and byte count.
type adapterRun struct {
	Name         string `json:"name"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at"`
	ExitCode     int    `json:"exit_code"`
	OutputSHA256 string `json:"output_sha256"`
	OutputBytes  int    `json:"output_bytes"`
	ReceiptPath  string `json:"receipt_path,omitempty"`
}

type releaseAdapter struct {
	Name          string
	Args          []string
	SuccessStatus string
}

var identityCriticalLaunchSecrets = []string{
	"MERC_TOKEN_KEY", "MERC_VERIFICATION_SAMPLE_SECRET", "STRIPE_WEBHOOK_SECRET", "MERC_CONNECT_WEBHOOK_SECRET",
}

type launchInputs struct {
	SchemaVersion int `json:"schema_version"`
	Missing       []struct {
		Name     string `json:"name"`
		Secret   bool   `json:"secret"`
		UsedBy   string `json:"used_by"`
		Accepted string `json:"accepted_form"`
	} `json:"missing"`
	Ready bool `json:"ready"`
}

type launchInput struct {
	Name     string `json:"name"`
	Secret   bool   `json:"secret"`
	UsedBy   string `json:"used_by"`
	Accepted string `json:"accepted_form"`
}

type launchInputContract struct {
	SchemaVersion int           `json:"schema_version"`
	Inputs        []launchInput `json:"inputs"`
}

// launchInputBundle is the operator-facing reduction of the detailed adapter
// contract.  The detailed names remain the runtime authority; this view makes
// clear that they are supplied through eight external decisions, rather than
// forty-six unrelated prompts.
type launchInputBundle struct {
	ID          string   `json:"id"`
	Class       string   `json:"class"`
	Gate        string   `json:"gate"`
	Inputs      []string `json:"inputs"`
	SecretStore string   `json:"secret_store"`
}

var minimalLaunchInputBundles = []launchInputBundle{
	{ID: "staging", Class: "EXTERNAL_EVENT", Gate: "persistent TLS staging", SecretStore: ".merc-launch.env (0600) for credentials; level-b.yaml for topology", Inputs: []string{"STAGING_SSH_TARGET", "STAGING_TLS_HOSTNAME", "STAGING_DEPLOYMENT_ROOT", "STAGING_BIND_ADDRESS", "ACME_EMAIL", "POSTGRES_PASSWORD", "MERC_TOKEN_KEY", "MERC_VERIFICATION_SAMPLE_SECRET", "MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE", "MERC_PRICE_FX_REVISION", "MERC_CANDIDATE_CONTROL_IMAGE", "MERC_PRIOR_CONTROL_IMAGE", "MERC_PRIOR_COMMIT", "MERC_PROMETHEUS_IMAGE", "MERC_ALERTMANAGER_IMAGE", "MERC_GRAFANA_IMAGE", "MERC_NODE_EXPORTER_IMAGE"}},
	{ID: "artifact-storage", Class: "EXTERNAL_EVENT", Gate: "persistent artifact storage", SecretStore: ".merc-launch.env (0600)", Inputs: []string{"STAGING_STORAGE_TLS_HOSTNAME", "MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD", "GF_SECURITY_ADMIN_PASSWORD"}},
	{ID: "offsite-backup", Class: "EXTERNAL_EVENT", Gate: "independent encrypted backup and restore", SecretStore: ".merc-launch.env (0600); age identity in separate 0600 file", Inputs: []string{"MERC_BACKUP_OFFSITE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "MERC_BACKUP_ENCRYPTION_RECIPIENT", "MERC_BACKUP_DECRYPTION_IDENTITY_FILE"}},
	{ID: "stripe-sandbox", Class: "EXTERNAL_EVENT", Gate: "Stripe CAD sandbox matrix", SecretStore: ".merc-launch.env (0600)", Inputs: []string{"STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET", "MERC_CONNECT_WEBHOOK_SECRET", "MERC_CONNECT_CLIENT_ID", "STRIPE_TEST_CONNECTED_ACCOUNT_ID", "STRIPE_BILLING_WEBHOOK_ENDPOINT_ID", "STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID"}},
	{ID: "alerting", Class: "EXTERNAL_EVENT", Gate: "real alert delivery and resolution", SecretStore: ".merc-launch.env (0600)", Inputs: []string{"ALERT_RECEIVER_WEBHOOK_URL", "ALERT_RECEIVER_NAME"}},
	{ID: "participants", Class: "HUMAN_APPROVAL", Gate: "approved buyer/worker canary", SecretStore: "ops/launch/participants.template.json completed outside git", Inputs: []string{"MERC_CANARY_APPROVED_BUYER_EMAILS", "MERC_CANARY_APPROVED_WORKER_IDS", "MERC_CANARY_APPROVED_AGENT_VERSIONS", "MERC_CANARY_APPROVED_BUILD_HASHES", "MERC_CANARY_SCENARIO_DRIVER", "MERC_CANARY_APPROVED_DRIVER_SHA256", "MERC_AGENT_RESTART_DRIVER", "MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256"}},
	{ID: "independent-review", Class: "HUMAN_APPROVAL", Gate: "independent accountable review and retest", SecretStore: "approved reviewer record outside git", Inputs: []string{"GITHUB_RELEASE_REVIEWER_LOGIN"}},
	{ID: "governance", Class: "HUMAN_APPROVAL", Gate: "candidate-bound governance approvals", SecretStore: "restricted governance evidence store", Inputs: []string{"GOVERNANCE_APPROVAL_BUNDLE_PATH"}},
}

var launchInputClassifications = map[string]string{
	"MERC_CANDIDATE_COMMIT":        "AUTO_DISCOVER",
	"STAGING_STORAGE_TLS_HOSTNAME": "DERIVE_FROM_OTHER_INPUT",
	"STAGING_BIND_ADDRESS":         "OPERATOR_DECISION",
}

func releaseRepoRoot() string {
	if root := strings.TrimSpace(os.Getenv("MERC_REPO_ROOT")); root != "" {
		return root
	}
	wd, err := os.Getwd()
	if err != nil {
		fatalf("release working directory: %v", err)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			fatalf("release must run inside the Merc repository (or set MERC_REPO_ROOT)")
		}
	}
}

func loadLaunchSecrets(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%s must have mode 0600", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for lineNo, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || name == "" || strings.TrimSpace(name) != name {
			return nil, fmt.Errorf("%s:%d must be NAME=VALUE", path, lineNo+1)
		}
		for i, r := range name {
			if !(r == '_' || r >= 'A' && r <= 'Z' || (i > 0 && r >= '0' && r <= '9')) {
				return nil, fmt.Errorf("%s:%d has invalid variable name", path, lineNo+1)
			}
		}
		if _, exists := values[name]; exists {
			return nil, fmt.Errorf("%s:%d duplicates %s", path, lineNo+1, name)
		}
		values[name] = value
	}
	return values, nil
}

func loadLaunchConfig(path, environment string) (launchConfig, []byte, error) {
	var cfg launchConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, nil, fmt.Errorf("parse launch config: %w", err)
	}
	if cfg.SchemaVersion != 1 || cfg.Environment != environment || environment != levelBEnvironment {
		return cfg, nil, errors.New("launch config must be schema_version: 1 and environment: staging")
	}
	canonical, err := canonicalProofJSON(cfg)
	return cfg, canonical, err
}

func launchConfigValues(cfg launchConfig) map[string]string {
	join := func(values []string) string { return strings.Join(values, ",") }
	storageHostname := cfg.Staging.StorageHostname
	if storageHostname == "" && cfg.Staging.TLSHostname != "" {
		// The private object endpoint is mechanically derived from the selected
		// staging base name; the deployment check still requires DNS/TLS proof.
		storageHostname = "objects." + cfg.Staging.TLSHostname
	}
	return map[string]string{
		"MERC_CANDIDATE_CONTROL_IMAGE": cfg.Candidate.ControlImage, "MERC_PRIOR_CONTROL_IMAGE": cfg.Candidate.PriorImage, "MERC_PRIOR_COMMIT": cfg.Candidate.PriorCommit,
		"STAGING_SSH_TARGET": cfg.Staging.SSHTarget, "STAGING_TLS_HOSTNAME": cfg.Staging.TLSHostname, "STAGING_DEPLOYMENT_ROOT": cfg.Staging.DeploymentRoot, "STAGING_STORAGE_TLS_HOSTNAME": storageHostname, "STAGING_BIND_ADDRESS": cfg.Staging.BindAddress, "ACME_EMAIL": cfg.Staging.ACMEEmail,
		"MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE": cfg.Pricing.ReferenceToSettlementRate, "MERC_PRICE_FX_REVISION": cfg.Pricing.FXRevision,
		"MERC_PROMETHEUS_IMAGE": cfg.Images.Prometheus, "MERC_ALERTMANAGER_IMAGE": cfg.Images.Alertmanager, "MERC_GRAFANA_IMAGE": cfg.Images.Grafana, "MERC_NODE_EXPORTER_IMAGE": cfg.Images.NodeExporter,
		"MERC_BACKUP_OFFSITE": cfg.Backup.Offsite, "MERC_BACKUP_ENCRYPTION_RECIPIENT": cfg.Backup.EncryptionRecipient,
		"MERC_CONNECT_CLIENT_ID": cfg.Stripe.ConnectClientID, "STRIPE_TEST_CONNECTED_ACCOUNT_ID": cfg.Stripe.TestConnectedAccountID, "STRIPE_BILLING_WEBHOOK_ENDPOINT_ID": cfg.Stripe.BillingWebhookEndpointID, "STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID": cfg.Stripe.ConnectWebhookEndpointID,
		"MERC_CANARY_APPROVED_BUYER_EMAILS": join(cfg.Canary.ApprovedBuyerEmails), "MERC_CANARY_APPROVED_WORKER_IDS": join(cfg.Canary.ApprovedWorkerIDs), "MERC_CANARY_APPROVED_AGENT_VERSIONS": join(cfg.Canary.ApprovedAgentVersions), "MERC_CANARY_APPROVED_BUILD_HASHES": join(cfg.Canary.ApprovedBuildHashes), "MERC_CANARY_SCENARIO_DRIVER": cfg.Canary.ScenarioDriver, "MERC_CANARY_APPROVED_DRIVER_SHA256": cfg.Canary.ScenarioDriverSHA256, "MERC_AGENT_RESTART_DRIVER": cfg.Canary.RestartDriver, "MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256": cfg.Canary.RestartDriverSHA256,
		"ALERT_RECEIVER_NAME": cfg.Alert.ReceiverName, "GITHUB_RELEASE_REVIEWER_LOGIN": cfg.Review.GitHubReviewerLogin, "GOVERNANCE_APPROVAL_BUNDLE_PATH": cfg.Governance.ApprovalBundlePath,
	}
}

func launchConfigValuesForRoot(root string, cfg launchConfig) (map[string]string, error) {
	values := launchConfigValues(cfg)
	if cfg.Candidate.Commit != "" {
		values["MERC_CANDIDATE_COMMIT"] = cfg.Candidate.Commit
		return values, nil
	}
	fp, err := launchSource(root)
	if err != nil {
		return nil, err
	}
	values["MERC_CANDIDATE_COMMIT"] = fp.Head
	return values, nil
}

func mergeLaunchValues(config, secrets map[string]string) map[string]string {
	out := make(map[string]string, len(config)+len(secrets))
	for name, value := range config {
		out[name] = value
	}
	for name, value := range secrets {
		out[name] = value
	}
	return out
}

func launchSource(root string) (sourceFingerprintResult, error) { return sourceFingerprint(root) }

func sha256Hex(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func secretNameDigest(values map[string]string) string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return sha256Hex([]byte(strings.Join(names, "\n")))
}

func loadLaunchInputContract(root string) (launchInputContract, error) {
	var contract launchInputContract
	raw, err := os.ReadFile(filepath.Join(root, "ops", "go-closure-inputs.json"))
	if err != nil {
		return contract, err
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		return contract, fmt.Errorf("parse input contract: %w", err)
	}
	if contract.SchemaVersion != 1 || len(contract.Inputs) == 0 {
		return contract, errors.New("unsupported input contract schema")
	}
	return contract, nil
}

func remoteProfileDigest(root string, values map[string]string) (string, error) {
	contract, err := loadLaunchInputContract(root)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(contract.Inputs))
	for _, input := range contract.Inputs {
		names = append(names, input.Name)
	}
	sort.Strings(names)
	var material strings.Builder
	material.WriteString("merc-level-b-remote-profile-v1\x00")
	for _, name := range names {
		material.WriteString(name)
		material.WriteByte(0)
		material.WriteString(values[name])
		material.WriteByte(0)
	}
	return sha256Hex([]byte(material.String())), nil
}

func identitySecretFingerprints(values map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(identityCriticalLaunchSecrets))
	for _, name := range identityCriticalLaunchSecrets {
		value := values[name]
		if strings.TrimSpace(value) == "" {
			continue
		}
		// A domain-separated digest makes a value useful only for continuity
		// comparison; no plaintext is emitted or persisted.
		out[name] = sha256Hex([]byte("merc-level-b-secret-fingerprint-v1\x00" + name + "\x00" + value))
	}
	return out, nil
}

func releaseStatePath(root string) string { return filepath.Join(root, ".merc-release", "state.json") }

func writeLaunchState(root string, state launchState) error {
	dir := filepath.Dir(releaseStatePath(root))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	raw, err := canonicalProofJSON(state)
	if err != nil {
		return err
	}
	return atomicWrite(releaseStatePath(root), append(raw, '\n'), 0o600)
}

func readLaunchState(root string) (launchState, error) {
	var state launchState
	raw, err := os.ReadFile(releaseStatePath(root))
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, fmt.Errorf("parse release state: %w", err)
	}
	if state.SchemaVersion != 1 && state.SchemaVersion != 2 || state.Status == "" || state.Plan.PlanSHA256 == "" {
		return state, errors.New("invalid release state")
	}
	return state, nil
}

func compileLaunchPlan(root, environment, configPath, secretsPath string) (launchPlan, error) {
	cfg, configBytes, err := loadLaunchConfig(configPath, environment)
	if err != nil {
		return launchPlan{}, err
	}
	secrets, err := loadLaunchSecrets(secretsPath)
	if err != nil {
		return launchPlan{}, err
	}
	fp, err := launchSource(root)
	if err != nil {
		return launchPlan{}, err
	}
	if fp.Dirty {
		return launchPlan{}, errors.New("refusing to seal a plan from a dirty source tree")
	}
	commit := cfg.Candidate.Commit
	if commit == "" {
		commit = fp.Head
	}
	if commit != fp.Head {
		return launchPlan{}, fmt.Errorf("config candidate commit %s is not exact HEAD %s", commit, fp.Head)
	}
	configValues, err := launchConfigValuesForRoot(root, cfg)
	if err != nil {
		return launchPlan{}, err
	}
	configValues["MERC_CANDIDATE_COMMIT"] = commit
	inputs, err := buildLaunchInputs(root, mergeLaunchValues(configValues, secrets))
	if err != nil {
		return launchPlan{}, err
	}
	if !inputs.Ready {
		return launchPlan{}, fmt.Errorf("launch configuration/input bundle incomplete: %d required entries missing (run merc release inputs)", len(inputs.Missing))
	}
	plan := launchPlan{SchemaVersion: 1, Kind: "merc_level_b_release_plan", Environment: environment,
		CandidateCommit: commit, SourceSHA256: fp.SourceSHA256, ConfigSHA256: sha256Hex(configBytes),
		SecretNamesSHA256: secretNameDigest(secrets), LiveMoneyProhibited: true,
		RequiredCommands: []string{"doctor", "apply", "canary", "soak", "prove", "go-no-go"}}
	plan.RemoteProfileSHA256, err = remoteProfileDigest(root, mergeLaunchValues(configValues, secrets))
	if err != nil {
		return launchPlan{}, err
	}
	plan.IdentityFingerprints, err = identitySecretFingerprints(secrets)
	if err != nil {
		return launchPlan{}, err
	}
	unsigned, err := canonicalProofJSON(plan)
	if err != nil {
		return launchPlan{}, err
	}
	plan.PlanSHA256 = sha256Hex(unsigned)
	return plan, nil
}

func printLaunch(v any) {
	raw, err := canonicalProofJSON(v)
	if err != nil {
		fatalf("encode release output: %v", err)
	}
	fmt.Println(string(raw))
}

func runReleaseDoctor(root string, secrets map[string]string) ([]byte, error) {
	cmd := exec.Command(filepath.Join(root, "scripts", "release-doctor.sh"), "--json")
	cmd.Dir = root
	// Never allow an ambient live credential or a developer .env to influence a
	// Level-B doctor result. The launch file is the complete secret authority.
	cmd.Env = scrubbedReleaseEnv(os.Environ())
	cmd.Env = append(cmd.Env, "MERC_RELEASE_DOCTOR_NO_ENV_FILE=1")
	for name, value := range secrets {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	return cmd.Output()
}

type remoteReleaseIdentity struct {
	SchemaVersion   int    `json:"schema_version"`
	Kind            string `json:"kind"`
	Status          string `json:"status"`
	ProfileSHA256   string `json:"profile_sha256"`
	CandidateCommit string `json:"candidate_commit"`
	ControlImage    string `json:"control_image"`
}

func runRemoteReleaseIdentity(root, envFile string, plan launchPlan) error {
	cmd := exec.Command(filepath.Join(root, "scripts", "go-closure-release-identity.sh"), "--target", "ssh")
	cmd.Dir = root
	cmd.Env = append(scrubbedReleaseEnv(os.Environ()), "MERC_GO_CLOSURE_ENV_FILE="+envFile)
	output, err := cmd.Output()
	if err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return fmt.Errorf("remote staging identity probe failed (exit=%d output_sha256=%s)", exitCode, sha256Hex(output))
	}
	var identity remoteReleaseIdentity
	if err := json.Unmarshal(output, &identity); err != nil {
		return fmt.Errorf("parse remote staging identity: %w", err)
	}
	if identity.SchemaVersion != 1 || identity.Kind != "merc_level_b_remote_input_profile" || identity.Status != "PASS" ||
		len(identity.ProfileSHA256) != 64 || identity.CandidateCommit != plan.CandidateCommit {
		return errors.New("remote staging identity response is incomplete or candidate-mismatched")
	}
	if identity.ProfileSHA256 != plan.RemoteProfileSHA256 {
		return errors.New("remote staging input profile drifted from sealed plan; reseal only after reconciling the host and operator files")
	}
	return nil
}

var adapterReceiptLine = regexp.MustCompile(`(?m)PASS receipt: ([^\r\n[:space:]]+)`)

func safeRemoteEvidencePath(deploymentRoot, value string) (string, error) {
	clean := strings.TrimSpace(value)
	if clean == "" || strings.Contains(clean, "\\") || strings.Contains(clean, "\x00") {
		return "", errors.New("receipt path is empty or unsafe")
	}
	deploymentRoot = filepath.ToSlash(filepath.Clean(deploymentRoot))
	if filepath.IsAbs(clean) {
		// Adapter output originates on staging; compare lexical POSIX paths here
		// because the local workstation need not mount the remote root.
		clean = filepath.ToSlash(clean)
		remoteRoot := filepath.ToSlash(deploymentRoot)
		if !strings.HasPrefix(clean, remoteRoot+"/") {
			return "", errors.New("adapter receipt is outside the staging root")
		}
		clean = strings.TrimPrefix(clean, remoteRoot+"/")
	}
	if strings.HasPrefix(clean, "/") || strings.Contains(clean, "..") || !strings.HasPrefix(clean, "evidence/go-closure/") || !strings.HasSuffix(clean, ".json") {
		return "", errors.New("adapter receipt must be a safe evidence/go-closure JSON path")
	}
	return clean, nil
}

func extractAdapterReceipt(deploymentRoot string, output []byte) (string, error) {
	matches := adapterReceiptLine.FindAllSubmatch(output, -1)
	if len(matches) != 1 {
		return "", fmt.Errorf("adapter must emit exactly one PASS receipt line, got %d", len(matches))
	}
	return safeRemoteEvidencePath(deploymentRoot, string(matches[0][1]))
}

func stateReceiptPath(state launchState, name string) (string, error) {
	for index := len(state.AdapterHistory) - 1; index >= 0; index-- {
		run := state.AdapterHistory[index]
		if run.Name == name && run.ExitCode == 0 && run.ReceiptPath != "" {
			return run.ReceiptPath, nil
		}
	}
	return "", fmt.Errorf("no exact successful receipt recorded for %s", name)
}

type releaseRootEvidence struct {
	SchemaVersion        int               `json:"schema_version"`
	Kind                 string            `json:"kind"`
	Status               string            `json:"status"`
	PlanSHA256           string            `json:"plan_sha256"`
	CandidateCommit      string            `json:"candidate_commit"`
	RemoteProfileSHA256  string            `json:"remote_profile_sha256"`
	Receipts             map[string]string `json:"receipts"`
	EvidenceChainSHA256  string            `json:"evidence_chain_sha256"`
	EvidenceChain        json.RawMessage   `json:"evidence_chain"`
	SecretValuesRecorded bool              `json:"secret_values_recorded"`
}

func releaseEvidencePath(root string) string {
	return filepath.Join(root, ".merc-release", "evidence.json")
}

func exactEvidenceReceiptSet(state launchState, cfg launchConfig) (map[string]string, error) {
	items := []struct{ key, adapter string }{
		{"deploy", "deploy-candidate"}, {"rollback", "rollback-forward-rehearsal"},
		{"restart", "restart-storm"}, {"canary", "private-canary"}, {"soak", "qualifying-soak"},
	}
	receipts := make(map[string]string, len(items)+1)
	for _, item := range items {
		path, err := stateReceiptPath(state, item.adapter)
		if err != nil {
			return nil, err
		}
		receipts[item.key] = path
	}
	governance, err := safeRemoteEvidencePath(cfg.Staging.DeploymentRoot, cfg.Evidence.GovernanceReceipt)
	if err != nil {
		return nil, fmt.Errorf("governance receipt: %w", err)
	}
	receipts["governance"] = governance
	return receipts, nil
}

func runRemoteEvidenceProof(root, envFile string, plan launchPlan, state launchState, cfg launchConfig) (releaseRootEvidence, error) {
	var rootEvidence releaseRootEvidence
	receipts, err := exactEvidenceReceiptSet(state, cfg)
	if err != nil {
		return rootEvidence, err
	}
	cmd := exec.Command(filepath.Join(root, "scripts", "go-closure-evidence-proof.sh"),
		"--target", "ssh", "--deploy", receipts["deploy"], "--rollback", receipts["rollback"],
		"--restart", receipts["restart"], "--canary", receipts["canary"], "--soak", receipts["soak"], "--governance", receipts["governance"])
	cmd.Dir = root
	cmd.Env = append(scrubbedReleaseEnv(os.Environ()), "MERC_GO_CLOSURE_ENV_FILE="+envFile)
	output, err := cmd.Output()
	if err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return rootEvidence, fmt.Errorf("remote evidence proof failed (exit=%d output_sha256=%s)", exitCode, sha256Hex(output))
	}
	var chain struct {
		SchemaVersion int    `json:"schema_version"`
		Kind          string `json:"kind"`
		Status        string `json:"status"`
		Candidate     string `json:"candidate_commit"`
	}
	if err := json.Unmarshal(output, &chain); err != nil {
		return rootEvidence, fmt.Errorf("parse remote evidence proof: %w", err)
	}
	if chain.SchemaVersion != 1 || chain.Kind != "merc_go_closure_evidence_chain_validation" || chain.Status != "PASS" || chain.Candidate != plan.CandidateCommit {
		return rootEvidence, errors.New("remote evidence proof is incomplete or candidate-mismatched")
	}
	rootEvidence = releaseRootEvidence{SchemaVersion: 1, Kind: "merc_level_b_release_root_evidence", Status: "PASS",
		PlanSHA256: plan.PlanSHA256, CandidateCommit: plan.CandidateCommit, RemoteProfileSHA256: plan.RemoteProfileSHA256,
		Receipts: receipts, EvidenceChainSHA256: sha256Hex(output), EvidenceChain: append(json.RawMessage(nil), output...), SecretValuesRecorded: false}
	return rootEvidence, nil
}

func writeReleaseEvidence(root string, evidence releaseRootEvidence) error {
	if err := os.MkdirAll(filepath.Dir(releaseEvidencePath(root)), 0o700); err != nil {
		return err
	}
	raw, err := canonicalProofJSON(evidence)
	if err != nil {
		return err
	}
	return atomicWrite(releaseEvidencePath(root), append(raw, '\n'), 0o600)
}

func runReadinessDecision(root string) (string, string, error) {
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "validate-readiness.py"))
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("readiness validation failed (output_sha256=%s)", sha256Hex(output))
	}
	text := string(output)
	if strings.Contains(text, "Level B GO") {
		return "GO", text, nil
	}
	return "NO_GO", text, nil
}

func shellQuoteEnvValue(value string) string {
	// The audited adapters source their operator file with POSIX shell.  Single
	// quoting every value prevents an operator-supplied dollar, backtick, or
	// command substitution from changing launch semantics.
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func writeAdapterEnv(root string, values map[string]string) (string, func(), error) {
	dir := filepath.Join(root, ".merc-release")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	file, err := os.CreateTemp(dir, "adapter-env-*")
	if err != nil {
		return "", nil, err
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, err
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := fmt.Fprintf(file, "%s=%s\n", name, shellQuoteEnvValue(values[name])); err != nil {
			_ = file.Close()
			cleanup()
			return "", nil, err
		}
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

func releaseAdapters(operation string) ([]releaseAdapter, error) {
	ssh := []string{"--target", "ssh"}
	switch operation {
	case "apply":
		return []releaseAdapter{{Name: "deploy-candidate", Args: append(ssh, "--activate", "candidate", "--execute"), SuccessStatus: "applied"}}, nil
	case "rollback":
		return []releaseAdapter{{Name: "rollback-forward-rehearsal", Args: append(ssh, "--execute"), SuccessStatus: "rollback_rehearsed"}}, nil
	case "canary":
		return []releaseAdapter{
			{Name: "restart-storm", Args: append(ssh, "--execute"), SuccessStatus: "restart_rehearsed"},
			{Name: "private-canary", Args: append(ssh, "--execute"), SuccessStatus: "canary_complete"},
		}, nil
	case "soak":
		return []releaseAdapter{{Name: "qualifying-soak", Args: append(ssh, "--duration", "86400", "--execute"), SuccessStatus: "soak_complete"}}, nil
	case "launch":
		return []releaseAdapter{
			{Name: "deploy-candidate", Args: append(ssh, "--activate", "candidate", "--execute"), SuccessStatus: "applied"},
			{Name: "rollback-forward-rehearsal", Args: append(ssh, "--execute"), SuccessStatus: "rollback_rehearsed"},
			{Name: "restart-storm", Args: append(ssh, "--execute"), SuccessStatus: "restart_rehearsed"},
			{Name: "private-canary", Args: append(ssh, "--execute"), SuccessStatus: "canary_complete"},
			{Name: "qualifying-soak", Args: append(ssh, "--duration", "86400", "--execute"), SuccessStatus: "soak_complete"},
		}, nil
	case "destroy":
		return []releaseAdapter{{Name: "controlled-teardown", Args: append(ssh, "--execute"), SuccessStatus: "destroyed"}}, nil
	}
	return nil, fmt.Errorf("no audited adapter for release %s", operation)
}

func adapterScript(name string) (string, error) {
	switch name {
	case "deploy-candidate":
		return "go-closure-deploy.sh", nil
	case "rollback-forward-rehearsal":
		return "go-closure-rollback-rehearsal.sh", nil
	case "restart-storm":
		return "go-closure-restart-storm.sh", nil
	case "private-canary":
		return "go-closure-canary-rehearsal.sh", nil
	case "qualifying-soak":
		return "go-closure-soak.sh", nil
	case "controlled-teardown":
		return "go-closure-destroy.sh", nil
	default:
		return "", fmt.Errorf("unknown audited adapter %s", name)
	}
}

func adapterFailureStatus(name string) string {
	switch name {
	case "deploy-candidate":
		return "apply_failed"
	case "rollback-forward-rehearsal":
		return "rollback_failed"
	case "restart-storm", "private-canary":
		return "canary_failed"
	case "qualifying-soak":
		return "soak_failed"
	case "controlled-teardown":
		return "destroy_failed"
	default:
		return "adapter_failed"
	}
}

func operationAllowed(status, operation string) bool {
	switch operation {
	case "apply":
		return status == "planned" || status == "apply_failed"
	case "rollback":
		return status == "applied" || status == "rollback_failed"
	case "canary":
		return status == "rollback_rehearsed" || status == "restart_rehearsed" || status == "canary_failed"
	case "soak":
		return status == "canary_complete" || status == "soak_failed"
	case "launch":
		return status == "planned" || strings.HasSuffix(status, "_failed")
	case "destroy":
		switch status {
		case "planned", "applied", "rollback_rehearsed", "restart_rehearsed", "canary_complete", "soak_complete",
			"apply_failed", "rollback_failed", "canary_failed", "soak_failed", "destroy_failed":
			return true
		}
	}
	return false
}

func resumeOperation(status string) (string, error) {
	switch status {
	case "planned", "apply_failed":
		return "apply", nil
	case "applied", "rollback_failed":
		return "rollback", nil
	case "rollback_rehearsed", "restart_rehearsed", "canary_failed":
		return "canary", nil
	case "canary_complete", "soak_failed":
		return "soak", nil
	case "destroy_failed":
		return "", errors.New("controlled teardown must be retried explicitly with --confirm-destroy")
	}
	return "", fmt.Errorf("state %q has no safe resumable operation", status)
}

func executeReleaseAdapters(root string, state launchState, operation string, values map[string]string) (launchState, error) {
	if !operationAllowed(state.Status, operation) {
		return state, fmt.Errorf("release %s is not allowed from state %q", operation, state.Status)
	}
	if operation == "launch" && state.Status != "planned" {
		return state, fmt.Errorf("release launch only starts from planned; use resume from state %q", state.Status)
	}
	adapters, err := releaseAdapters(operation)
	if err != nil {
		return state, err
	}
	envFile, cleanup, err := writeAdapterEnv(root, values)
	if err != nil {
		return state, fmt.Errorf("prepare adapter environment: %w", err)
	}
	defer cleanup()
	if err := runRemoteReleaseIdentity(root, envFile, state.Plan); err != nil {
		return state, err
	}
	for _, adapter := range adapters {
		script, err := adapterScript(adapter.Name)
		if err != nil {
			return state, err
		}
		state.Status = adapter.Name + "_running"
		state.LastOperation = operation
		if err := writeLaunchState(root, state); err != nil {
			return state, fmt.Errorf("record running adapter: %w", err)
		}
		started := time.Now().UTC()
		cmd := exec.Command(filepath.Join(root, "scripts", script), adapter.Args...)
		cmd.Dir = root
		cmd.Env = append(scrubbedReleaseEnv(os.Environ()), "MERC_GO_CLOSURE_ENV_FILE="+envFile)
		output, runErr := cmd.CombinedOutput()
		finished := time.Now().UTC()
		exitCode := 0
		if runErr != nil {
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}
		run := adapterRun{
			Name: adapter.Name, StartedAt: started.Format(time.RFC3339), FinishedAt: finished.Format(time.RFC3339),
			ExitCode: exitCode, OutputSHA256: sha256Hex(output), OutputBytes: len(output),
		}
		if runErr != nil {
			state.AdapterHistory = append(state.AdapterHistory, run)
			state.Status = adapterFailureStatus(adapter.Name)
			if err := writeLaunchState(root, state); err != nil {
				return state, fmt.Errorf("record failed adapter: %w", err)
			}
			return state, fmt.Errorf("audited adapter %s failed (exit=%d output_sha256=%s)", adapter.Name, exitCode, sha256Hex(output))
		}
		receiptPath, receiptErr := extractAdapterReceipt(values["STAGING_DEPLOYMENT_ROOT"], output)
		if receiptErr != nil {
			run.ExitCode = -2
			state.AdapterHistory = append(state.AdapterHistory, run)
			state.Status = adapterFailureStatus(adapter.Name)
			if err := writeLaunchState(root, state); err != nil {
				return state, fmt.Errorf("record receipt-less adapter: %w", err)
			}
			return state, fmt.Errorf("audited adapter %s succeeded without an exact safe receipt: %w", adapter.Name, receiptErr)
		}
		run.ReceiptPath = receiptPath
		state.AdapterHistory = append(state.AdapterHistory, run)
		state.Status = adapter.SuccessStatus
		if err := writeLaunchState(root, state); err != nil {
			return state, fmt.Errorf("record successful adapter: %w", err)
		}
	}
	return state, nil
}

func scrubbedReleaseEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		name, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(name, "STRIPE_") || strings.HasPrefix(name, "MERC_") ||
			strings.HasPrefix(name, "AWS_") || strings.HasPrefix(name, "ALERT_") ||
			name == "POSTGRES_PASSWORD" || name == "MINIO_ROOT_USER" || name == "MINIO_ROOT_PASSWORD" || name == "GF_SECURITY_ADMIN_PASSWORD" {
			continue
		}
		out = append(out, item)
	}
	return out
}

func buildLaunchInputs(root string, values map[string]string) (launchInputs, error) {
	contract, err := loadLaunchInputContract(root)
	if err != nil {
		return launchInputs{}, err
	}
	var out launchInputs
	out.SchemaVersion = 1
	for _, in := range contract.Inputs {
		if strings.TrimSpace(values[in.Name]) == "" {
			out.Missing = append(out.Missing, struct {
				Name     string `json:"name"`
				Secret   bool   `json:"secret"`
				UsedBy   string `json:"used_by"`
				Accepted string `json:"accepted_form"`
			}(in))
		}
	}
	out.Ready = len(out.Missing) == 0
	return out, nil
}

func inputClassification(in launchInput) string {
	if classification := launchInputClassifications[in.Name]; classification != "" {
		return classification
	}
	if in.Secret {
		return "OPERATOR_SECRET"
	}
	return "OPERATOR_DECISION"
}

func cmdLaunchInputs(root, configPath, environment, secretsPath string, minimal, explain bool) {
	cfg, _, err := loadLaunchConfig(configPath, environment)
	if err != nil {
		fatalf("release inputs config: %v", err)
	}
	values, err := loadLaunchSecrets(secretsPath)
	if os.IsNotExist(err) {
		values, err = map[string]string{}, nil
	}
	if err != nil {
		fatalf("read secrets file: %v", err)
	}
	configValues, err := launchConfigValuesForRoot(root, cfg)
	if err != nil {
		fatalf("release inputs source identity: %v", err)
	}
	allValues := mergeLaunchValues(configValues, values)
	if minimal {
		bundles := make([]map[string]any, 0, len(minimalLaunchInputBundles))
		for _, bundle := range minimalLaunchInputBundles {
			missing := make([]string, 0, len(bundle.Inputs))
			for _, name := range bundle.Inputs {
				if strings.TrimSpace(allValues[name]) == "" && launchInputClassifications[name] != "AUTO_DISCOVER" && launchInputClassifications[name] != "DERIVE_FROM_OTHER_INPUT" {
					missing = append(missing, name)
				}
			}
			bundles = append(bundles, map[string]any{"id": bundle.ID, "class": bundle.Class, "gate": bundle.Gate,
				"secret_store": bundle.SecretStore, "missing": missing, "ready": len(missing) == 0})
		}
		printLaunch(map[string]any{"schema_version": 1, "kind": "merc_level_b_minimal_inputs", "irreducible_external_bundle_count": len(bundles),
			"external_events_not_inputs": []string{"24_hour_soak", "real_alert_firing_and_resolution", "external_backup_restore", "Stripe_CAD_matrix", "approved_canary_execution"}, "bundles": bundles})
		return
	}
	out, err := buildLaunchInputs(root, allValues)
	if err != nil {
		fatalf("release inputs: %v", err)
	}
	if explain {
		explanations := make([]map[string]any, 0, len(out.Missing))
		contract, contractErr := loadLaunchInputContract(root)
		if contractErr != nil {
			fatalf("release inputs explain: %v", contractErr)
		}
		for _, in := range contract.Inputs {
			explanations = append(explanations, map[string]any{"name": in.Name, "classification": inputClassification(in), "secret": in.Secret,
				"provided": strings.TrimSpace(allValues[in.Name]) != "", "used_by": in.UsedBy, "accepted_form": in.Accepted})
		}
		printLaunch(map[string]any{"schema_version": 1, "kind": "merc_level_b_input_explanation", "items": explanations})
		return
	}
	printLaunch(out)
}

func cmdReleaseEvidence(root, command, environment, configPath, secretsPath string) {
	plan, err := compileLaunchPlan(root, environment, configPath, secretsPath)
	if err != nil {
		fatalf("release %s: %v", command, err)
	}
	state, err := readLaunchState(root)
	if err != nil {
		fatalf("release %s requires a sealed release state: %v", command, err)
	}
	if state.Plan.PlanSHA256 != plan.PlanSHA256 || state.Plan.RemoteProfileSHA256 != plan.RemoteProfileSHA256 ||
		!equalStringMap(state.Plan.IdentityFingerprints, plan.IdentityFingerprints) {
		fatalf("release %s refused: sealed plan identity drifted; reseal and reapprove", command)
	}
	values, err := loadLaunchSecrets(secretsPath)
	if err != nil {
		fatalf("release %s: %v", command, err)
	}
	cfg, _, err := loadLaunchConfig(configPath, environment)
	if err != nil {
		fatalf("release %s config: %v", command, err)
	}
	configValues, err := launchConfigValuesForRoot(root, cfg)
	if err != nil {
		fatalf("release %s source identity: %v", command, err)
	}
	allValues := mergeLaunchValues(configValues, values)
	allValues["MERC_CANDIDATE_COMMIT"] = plan.CandidateCommit
	envFile, cleanup, err := writeAdapterEnv(root, allValues)
	if err != nil {
		fatalf("release %s adapter environment: %v", command, err)
	}
	defer cleanup()
	if err := runRemoteReleaseIdentity(root, envFile, plan); err != nil {
		fatalf("release %s: %v", command, err)
	}
	evidence, err := runRemoteEvidenceProof(root, envFile, plan, state, cfg)
	if err != nil {
		fatalf("release %s: %v", command, err)
	}
	if err := writeReleaseEvidence(root, evidence); err != nil {
		fatalf("release %s write root evidence: %v", command, err)
	}
	if command == "go-no-go" {
		decision, report, err := runReadinessDecision(root)
		if err != nil {
			fatalf("release go-no-go: %v", err)
		}
		printLaunch(map[string]any{"schema_version": 1, "kind": "merc_level_b_release_go_no_go",
			"level_b": decision, "level_c": "NO_GO_PROHIBITED", "root_evidence": evidence,
			"readiness_report_sha256": sha256Hex([]byte(report))})
		return
	}
	printLaunch(evidence)
}

func dispatchLaunchRelease(args []string) {
	command := args[0]
	root := releaseRepoRoot()
	fs := flag.NewFlagSet("release "+command, flag.ExitOnError)
	environment := fs.String("environment", levelBEnvironment, "release environment (staging only)")
	config := fs.String("config", filepath.Join(root, "ops", "launch", "level-b.yaml"), "launch config")
	secrets := fs.String("secrets-file", filepath.Join(root, ".merc-launch.env"), "mode-0600 secret file")
	approve := fs.String("approve-plan", "", "exact plan SHA-256 required for stateful commands")
	planOut := fs.String("out", "", "optional private path for the sealed plan JSON")
	apply := fs.Bool("apply", false, "acknowledge a stateful launch action")
	uiListen := fs.String("listen", "127.0.0.1:0", "loopback address for release ui")
	destroyConfirm := fs.String("confirm-destroy", "", "repeat exact plan SHA-256 to confirm controlled teardown")
	minimal := fs.Bool("minimal", false, "emit the eight external input bundles")
	explain := fs.Bool("explain", false, "classify every detailed launch input")
	_ = fs.Bool("json", true, "output canonical JSON (default)")
	fs.Parse(args[1:])
	if *environment != levelBEnvironment {
		fatalf("Level C and non-staging environments are prohibited")
	}
	switch command {
	case "inputs":
		cmdLaunchInputs(root, *config, *environment, *secrets, *minimal, *explain)
	case "doctor":
		cfg, _, err := loadLaunchConfig(*config, *environment)
		if err != nil {
			fatalf("release doctor config: %v", err)
		}
		values, err := loadLaunchSecrets(*secrets)
		if err != nil {
			fatalf("release doctor: %v", err)
		}
		configValues, err := launchConfigValuesForRoot(root, cfg)
		if err != nil {
			fatalf("release doctor source identity: %v", err)
		}
		out, err := runReleaseDoctor(root, mergeLaunchValues(configValues, values))
		if err != nil {
			fmt.Print(string(out))
			fatalf("Level B launch inputs are not ready")
		}
		fmt.Print(string(out))
	case "plan":
		plan, err := compileLaunchPlan(root, *environment, *config, *secrets)
		if err != nil {
			fatalf("release plan: %v", err)
		}
		if err := writeLaunchState(root, launchState{SchemaVersion: 2, Status: "planned", Plan: plan}); err != nil {
			fatalf("seal release plan: %v", err)
		}
		if strings.TrimSpace(*planOut) != "" {
			if filepath.IsAbs(*planOut) {
				fatalf("release plan --out must be a repository-relative path")
			}
			outputPath := filepath.Join(root, filepath.Clean(*planOut))
			relativeOutput, relErr := filepath.Rel(root, outputPath)
			if relErr != nil || relativeOutput == ".." || strings.HasPrefix(relativeOutput, ".."+string(filepath.Separator)) {
				fatalf("release plan --out escapes repository")
			}
			raw, encodeErr := canonicalProofJSON(plan)
			if encodeErr != nil {
				fatalf("encode release plan: %v", encodeErr)
			}
			if mkdirErr := os.MkdirAll(filepath.Dir(outputPath), 0o700); mkdirErr != nil {
				fatalf("create release plan directory: %v", mkdirErr)
			}
			if writeErr := atomicWrite(outputPath, append(raw, '\n'), 0o600); writeErr != nil {
				fatalf("write release plan: %v", writeErr)
			}
		}
		printLaunch(plan)
	case "status":
		state, err := readLaunchState(root)
		if err != nil {
			fatalf("release status: no sealed release state: %v", err)
		}
		printLaunch(state)
	case "prove", "evidence", "go-no-go":
		cmdReleaseEvidence(root, command, *environment, *config, *secrets)
	case "ui":
		if err := serveReleaseUI(root, *uiListen); err != nil {
			fatalf("release ui: %v", err)
		}
	case "render":
		plan, err := compileLaunchPlan(root, *environment, *config, *secrets)
		if err != nil {
			fatalf("release %s: %v", command, err)
		}
		printLaunch(map[string]any{"schema_version": 1, "kind": "merc_level_b_release_render", "plan": plan,
			"execution_order":  []string{"remote_input_profile", "deploy_candidate", "rollback_forward_rehearsal", "restart_storm", "private_canary", "qualifying_24h_soak", "exact_evidence_chain", "readiness_go_no_go"},
			"receipt_contract": "one exact PASS receipt per adapter plus the configured governance receipt; never latest or glob",
			"level_b":          "NO_GO until external receipts verify", "level_c": "NO_GO_PROHIBITED"})
	default:
		if !*apply {
			fatalf("release %s requires --apply; dry-run with merc release plan", command)
		}
		plan, err := compileLaunchPlan(root, *environment, *config, *secrets)
		if err != nil {
			fatalf("release %s: %v", command, err)
		}
		if *approve == "" || *approve != plan.PlanSHA256 {
			fatalf("release %s refuses mutation without --approve-plan %s", command, plan.PlanSHA256)
		}
		if command == "destroy" && *destroyConfirm != plan.PlanSHA256 {
			fatalf("release destroy requires --confirm-destroy %s; persistent volumes and evidence remain preserved", plan.PlanSHA256)
		}
		state, err := readLaunchState(root)
		if err != nil {
			fatalf("release %s requires a sealed merc release plan: %v", command, err)
		}
		if state.Plan.PlanSHA256 != plan.PlanSHA256 || state.Plan.IdentityFingerprints == nil ||
			!equalStringMap(state.Plan.IdentityFingerprints, plan.IdentityFingerprints) {
			fatalf("release %s refused: sealed plan or identity-critical secret fingerprints drifted; reseal and reapprove", command)
		}
		values, err := loadLaunchSecrets(*secrets)
		if err != nil {
			fatalf("release %s: %v", command, err)
		}
		cfg, _, err := loadLaunchConfig(*config, *environment)
		if err != nil {
			fatalf("release %s config: %v", command, err)
		}
		configValues, err := launchConfigValuesForRoot(root, cfg)
		if err != nil {
			fatalf("release %s source identity: %v", command, err)
		}
		allValues := mergeLaunchValues(configValues, values)
		allValues["MERC_CANDIDATE_COMMIT"] = plan.CandidateCommit
		if command == "launch" {
			if _, err := safeRemoteEvidencePath(cfg.Staging.DeploymentRoot, cfg.Evidence.GovernanceReceipt); err != nil {
				fatalf("release launch requires a safe exact governance receipt before mutation: %v", err)
			}
		}
		out, err := runReleaseDoctor(root, allValues)
		if err != nil {
			inputs, inputErr := buildLaunchInputs(root, allValues)
			if inputErr != nil {
				fatalf("release %s input contract: %v", command, inputErr)
			}
			printLaunch(map[string]any{"schema_version": 1, "status": "REFUSED", "command": command, "plan_sha256": plan.PlanSHA256, "reason": "external input bundle incomplete", "doctor": json.RawMessage(out), "missing_inputs": inputs.Missing, "resume": "supply only the missing inputs, reseal merc release plan, then resume with the new approved plan"})
			fatalf("release %s refused: external input bundle incomplete", command)
		}
		operation := command
		if command == "resume" {
			operation, err = resumeOperation(state.Status)
			if err != nil {
				fatalf("release resume: %v", err)
			}
		}
		state, err = executeReleaseAdapters(root, state, operation, allValues)
		if err != nil {
			fatalf("release %s: %v", command, err)
		}
		if command == "launch" {
			// The one-button path does not infer receipt names: every adapter
			// recorded its exact PASS receipt, and this call binds all of them to
			// the final evidence-chain and readiness decision.
			cmdReleaseEvidence(root, "go-no-go", *environment, *config, *secrets)
			return
		}
		printLaunch(map[string]any{"schema_version": 1, "status": state.Status, "operation": operation,
			"plan_sha256": plan.PlanSHA256, "level_b": "external receipts still require independent review", "level_c": "NO_GO_PROHIBITED"})
	}
}

func equalStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
