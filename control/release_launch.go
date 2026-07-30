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
	"sort"
	"strings"

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
}

type launchPlan struct {
	SchemaVersion        int               `json:"schema_version"`
	Kind                 string            `json:"kind"`
	Environment          string            `json:"environment"`
	CandidateCommit      string            `json:"candidate_commit"`
	SourceSHA256         string            `json:"source_sha256"`
	ConfigSHA256         string            `json:"config_sha256"`
	SecretNamesSHA256    string            `json:"secret_names_sha256"`
	IdentityFingerprints map[string]string `json:"identity_secret_fingerprints"`
	RequiredCommands     []string          `json:"required_commands"`
	LiveMoneyProhibited  bool              `json:"live_money_prohibited"`
	PlanSHA256           string            `json:"plan_sha256"`
}

type launchState struct {
	SchemaVersion int        `json:"schema_version"`
	Status        string     `json:"status"`
	Plan          launchPlan `json:"plan"`
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
	return map[string]string{
		"MERC_CANDIDATE_CONTROL_IMAGE": cfg.Candidate.ControlImage, "MERC_PRIOR_CONTROL_IMAGE": cfg.Candidate.PriorImage, "MERC_PRIOR_COMMIT": cfg.Candidate.PriorCommit,
		"STAGING_SSH_TARGET": cfg.Staging.SSHTarget, "STAGING_TLS_HOSTNAME": cfg.Staging.TLSHostname, "STAGING_DEPLOYMENT_ROOT": cfg.Staging.DeploymentRoot, "STAGING_STORAGE_TLS_HOSTNAME": cfg.Staging.StorageHostname, "STAGING_BIND_ADDRESS": cfg.Staging.BindAddress, "ACME_EMAIL": cfg.Staging.ACMEEmail,
		"MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE": cfg.Pricing.ReferenceToSettlementRate, "MERC_PRICE_FX_REVISION": cfg.Pricing.FXRevision,
		"MERC_PROMETHEUS_IMAGE": cfg.Images.Prometheus, "MERC_ALERTMANAGER_IMAGE": cfg.Images.Alertmanager, "MERC_GRAFANA_IMAGE": cfg.Images.Grafana, "MERC_NODE_EXPORTER_IMAGE": cfg.Images.NodeExporter,
		"MERC_BACKUP_OFFSITE": cfg.Backup.Offsite, "MERC_BACKUP_ENCRYPTION_RECIPIENT": cfg.Backup.EncryptionRecipient,
		"MERC_CONNECT_CLIENT_ID": cfg.Stripe.ConnectClientID, "STRIPE_TEST_CONNECTED_ACCOUNT_ID": cfg.Stripe.TestConnectedAccountID, "STRIPE_BILLING_WEBHOOK_ENDPOINT_ID": cfg.Stripe.BillingWebhookEndpointID, "STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID": cfg.Stripe.ConnectWebhookEndpointID,
		"MERC_CANARY_APPROVED_BUYER_EMAILS": join(cfg.Canary.ApprovedBuyerEmails), "MERC_CANARY_APPROVED_WORKER_IDS": join(cfg.Canary.ApprovedWorkerIDs), "MERC_CANARY_APPROVED_AGENT_VERSIONS": join(cfg.Canary.ApprovedAgentVersions), "MERC_CANARY_APPROVED_BUILD_HASHES": join(cfg.Canary.ApprovedBuildHashes), "MERC_CANARY_SCENARIO_DRIVER": cfg.Canary.ScenarioDriver, "MERC_CANARY_APPROVED_DRIVER_SHA256": cfg.Canary.ScenarioDriverSHA256, "MERC_AGENT_RESTART_DRIVER": cfg.Canary.RestartDriver, "MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256": cfg.Canary.RestartDriverSHA256,
		"ALERT_RECEIVER_NAME": cfg.Alert.ReceiverName, "GITHUB_RELEASE_REVIEWER_LOGIN": cfg.Review.GitHubReviewerLogin, "GOVERNANCE_APPROVAL_BUNDLE_PATH": cfg.Governance.ApprovalBundlePath,
	}
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
	if state.SchemaVersion != 1 || state.Status == "" || state.Plan.PlanSHA256 == "" {
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
	configValues := launchConfigValues(cfg)
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
	raw, err := os.ReadFile(filepath.Join(root, "ops", "go-closure-inputs.json"))
	if err != nil {
		return launchInputs{}, err
	}
	var contract struct {
		SchemaVersion int `json:"schema_version"`
		Inputs        []struct {
			Name     string `json:"name"`
			Secret   bool   `json:"secret"`
			UsedBy   string `json:"used_by"`
			Accepted string `json:"accepted_form"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		return launchInputs{}, fmt.Errorf("parse input contract: %w", err)
	}
	if contract.SchemaVersion != 1 {
		return launchInputs{}, errors.New("unsupported input contract schema")
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

func cmdLaunchInputs(root, configPath, environment, secretsPath string) {
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
	configValues := launchConfigValues(cfg)
	if configValues["MERC_CANDIDATE_COMMIT"] == "" {
		if fp, fpErr := launchSource(root); fpErr == nil {
			configValues["MERC_CANDIDATE_COMMIT"] = fp.Head
		}
	}
	out, err := buildLaunchInputs(root, mergeLaunchValues(configValues, values))
	if err != nil {
		fatalf("release inputs: %v", err)
	}
	printLaunch(out)
}

func dispatchLaunchRelease(args []string) {
	command := args[0]
	root := releaseRepoRoot()
	fs := flag.NewFlagSet("release "+command, flag.ExitOnError)
	environment := fs.String("environment", levelBEnvironment, "release environment (staging only)")
	config := fs.String("config", filepath.Join(root, "ops", "launch", "level-b.yaml"), "launch config")
	secrets := fs.String("secrets-file", filepath.Join(root, ".merc-launch.env"), "mode-0600 secret file")
	approve := fs.String("approve-plan", "", "exact plan SHA-256 required for stateful commands")
	apply := fs.Bool("apply", false, "acknowledge a stateful launch action")
	fs.Parse(args[1:])
	if *environment != levelBEnvironment {
		fatalf("Level C and non-staging environments are prohibited")
	}
	switch command {
	case "inputs":
		cmdLaunchInputs(root, *config, *environment, *secrets)
	case "doctor":
		cfg, _, err := loadLaunchConfig(*config, *environment)
		if err != nil {
			fatalf("release doctor config: %v", err)
		}
		values, err := loadLaunchSecrets(*secrets)
		if err != nil {
			fatalf("release doctor: %v", err)
		}
		out, err := runReleaseDoctor(root, mergeLaunchValues(launchConfigValues(cfg), values))
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
		if err := writeLaunchState(root, launchState{SchemaVersion: 1, Status: "planned", Plan: plan}); err != nil {
			fatalf("seal release plan: %v", err)
		}
		printLaunch(plan)
	case "status":
		state, err := readLaunchState(root)
		if err != nil {
			fatalf("release status: no sealed release state: %v", err)
		}
		printLaunch(state)
	case "render", "evidence", "go-no-go", "ui":
		plan, err := compileLaunchPlan(root, *environment, *config, *secrets)
		if err != nil {
			fatalf("release %s: %v", command, err)
		}
		printLaunch(map[string]any{"schema_version": 1, "kind": "merc_level_b_release_" + strings.ReplaceAll(command, "-", "_"), "plan": plan, "level_b": "NO_GO until external receipts verify", "level_c": "NO_GO_PROHIBITED"})
	default:
		if command == "launch" && !*apply {
			fatalf("release launch requires --apply; dry-run with merc release plan")
		}
		plan, err := compileLaunchPlan(root, *environment, *config, *secrets)
		if err != nil {
			fatalf("release %s: %v", command, err)
		}
		if *approve == "" || *approve != plan.PlanSHA256 {
			fatalf("release %s refuses mutation without --approve-plan %s", command, plan.PlanSHA256)
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
		allValues := mergeLaunchValues(launchConfigValues(cfg), values)
		out, err := runReleaseDoctor(root, allValues)
		if err != nil {
			inputs, inputErr := buildLaunchInputs(root, allValues)
			if inputErr != nil {
				fatalf("release %s input contract: %v", command, inputErr)
			}
			printLaunch(map[string]any{"schema_version": 1, "status": "REFUSED", "command": command, "plan_sha256": plan.PlanSHA256, "reason": "external input bundle incomplete", "doctor": json.RawMessage(out), "missing_inputs": inputs.Missing, "resume": "supply only the missing inputs, reseal merc release plan, then resume with the new approved plan"})
			fatalf("release %s refused: external input bundle incomplete", command)
		}
		fatalf("release %s adapter is not implemented; refusing to claim external execution", command)
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
