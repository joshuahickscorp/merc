package main

// The Level-B launcher is intentionally a thin, fail-closed authority over the
// existing audited release adapters.  It never manufactures external evidence
// or handles a secret value beyond loading a mode-0600 operator file into its
// own process environment.

import (
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
		Commit string `yaml:"commit" json:"commit"`
	} `yaml:"candidate" json:"candidate"`
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
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, nil, fmt.Errorf("parse launch config: %w", err)
	}
	if cfg.SchemaVersion != 1 || cfg.Environment != environment || environment != levelBEnvironment {
		return cfg, nil, errors.New("launch config must be schema_version: 1 and environment: staging")
	}
	canonical, err := canonicalProofJSON(cfg)
	return cfg, canonical, err
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

func cmdLaunchInputs(root, secretsPath string) {
	raw, err := os.ReadFile(filepath.Join(root, "ops", "go-closure-inputs.json"))
	if err != nil {
		fatalf("read input contract: %v", err)
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
	if err := json.Unmarshal(raw, &contract); err != nil || contract.SchemaVersion != 1 {
		fatalf("input contract invalid: %v", err)
	}
	values, err := loadLaunchSecrets(secretsPath)
	if os.IsNotExist(err) {
		values = map[string]string{}
		err = nil
	}
	if err != nil {
		fatalf("read secrets file: %v", err)
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
		cmdLaunchInputs(root, *secrets)
	case "doctor":
		values, err := loadLaunchSecrets(*secrets)
		if err != nil {
			fatalf("release doctor: %v", err)
		}
		out, err := runReleaseDoctor(root, values)
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
		out, err := runReleaseDoctor(root, values)
		if err != nil {
			printLaunch(map[string]any{"schema_version": 1, "status": "REFUSED", "command": command, "plan_sha256": plan.PlanSHA256, "reason": "external input bundle incomplete", "doctor": json.RawMessage(out), "resume": "supply only the missing fields reported by merc release doctor, then resume with the same exact plan"})
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
