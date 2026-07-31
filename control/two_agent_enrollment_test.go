package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Two real merc-agent processes enrolling autonomously against a real control
// plane.
//
// Every earlier checkpoint proved the production RUNNER and DRIVER generate valid
// bytes. None proved an enrolled worker process authenticates, registers,
// advertises a governed capability set and heartbeats on its own. Seeding a
// worker row and calling that enrolment would prove nothing about the agent.
//
// The agent binary is launched as a subprocess against an httptest server running
// the production Routes(), and every assertion reads what the CONTROL PLANE
// stored — not what the test told it. The only thing handed to each agent is a
// credential, which is what a supplier gets in production too.
//
// Writing this found four defects that code reading had not: the advertised
// engine was hardcoded to candle, the agent's capability projection was
// routable-only, worker profile resolution was routable-only, and agent.toml
// requires power_only with no serde default. Each was invisible until a real
// process tried to start.

// twoAgentEnrolTimeout is generous because each agent cold-loads and benchmarks
// BOTH retained models before it registers, and two agents on one host serialize
// on the GPU. Measured at roughly 45s alone.
const twoAgentEnrolTimeout = 300 * time.Second

func agentBinaryPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "agent", "target", "release", "merc-agent"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no release agent at %s; run `cargo build --release` in agent/", path)
	}
	return path
}

type enrolledAgent struct {
	name       string
	workerID   uuid.UUID
	supplierID uuid.UUID
	cmd        *exec.Cmd
	logPath    string
}

// launchAgent writes a config and starts a real merc-agent `run` process.
//
// Each agent gets its own HOME, data directory and log, so two agents on one host
// cannot share credentials or state — the isolation a two-supplier fleet has by
// construction and a test has to arrange.
func launchAgent(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	controlURL, name, embedRuntime, llamaURL string,
) *enrolledAgent {
	t.Helper()

	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		VALUES ($1,$2,'active',0.95,100)`,
		supplierID, name+"-"+uuid.NewString()+"@proof.test"); err != nil {
		t.Fatalf("%s: seed supplier: %v", name, err)
	}
	// The credential, and nothing else. The agent does the rest itself.
	token, err := store.CreateWorkerToken(ctx, workerID, supplierID)
	if err != nil {
		t.Fatalf("%s: issue worker token: %v", name, err)
	}

	home := t.TempDir()
	dataDir := filepath.Join(home, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "agent.toml")
	// Every field the agent requires. power_only has no serde default, so an
	// otherwise-valid config without it fails to parse before the process does
	// anything — which only a real process start reveals.
	config := fmt.Sprintf(`
control_url = %q
worker_token = %q
supplier_id = %q
data_dir = %q
power_only = false
min_payout_usd_per_hr = 0.0
memory_headroom_gb = 2.0
max_memory_pct = 95.0
checkpoint_secs = 30
embed_runtime = %q
llama_embed_base_url = %q
`, controlURL, token, supplierID, dataDir, embedRuntime, llamaURL)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(home, "agent.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(agentBinaryPath(t), "run", "--config", configPath)
	cmd.Dir = home
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		// The shared model cache: re-downloading pinned weights per agent would
		// make this a network test.
		"MERC_MODEL_CACHE="+filepath.Join(os.Getenv("HOME"), ".cache", "huggingface", "hub"),
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("%s: start agent: %v", name, err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		_ = logFile.Close()
		// A silent subprocess is the worst thing to debug.
		if t.Failed() {
			if body, err := os.ReadFile(logPath); err == nil {
				t.Logf("--- %s agent log ---\n%s", name, tailLines(string(body), 25))
			}
		}
	})

	return &enrolledAgent{
		name: name, workerID: workerID, supplierID: supplierID,
		cmd: cmd, logPath: logPath,
	}
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

type enrolment struct {
	engine    string
	profileID string
	revision  string
	digest    string
	cells     []string
	routable  []bool
}

// waitForEnrolment polls until the CONTROL PLANE has stored a governed identity
// and at least one authorized capability for this worker.
func waitForEnrolment(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, agent *enrolledAgent,
) enrolment {
	t.Helper()
	deadline := time.Now().Add(twoAgentEnrolTimeout)
	for time.Now().Before(deadline) {
		var out enrolment
		var pid, rev, dig *string
		err := pool.QueryRow(ctx, `
			SELECT engine, runtime_profile_id, runtime_profile_revision, runtime_profile_digest
			  FROM workers WHERE id=$1`, agent.workerID).Scan(&out.engine, &pid, &rev, &dig)
		if err == nil && pid != nil && rev != nil && dig != nil {
			rows, qerr := pool.Query(ctx, `
				SELECT cell_id, routable FROM worker_authorized_capabilities
				 WHERE worker_id=$1 ORDER BY cell_id`, agent.workerID)
			if qerr == nil {
				for rows.Next() {
					var cell string
					var routable bool
					if rows.Scan(&cell, &routable) == nil {
						out.cells = append(out.cells, cell)
						out.routable = append(out.routable, routable)
					}
				}
				rows.Close()
			}
			if len(out.cells) > 0 {
				out.profileID, out.revision, out.digest = *pid, *rev, *dig
				return out
			}
		}
		if agent.cmd.ProcessState != nil && agent.cmd.ProcessState.Exited() {
			t.Fatalf("%s agent exited before enrolling (%s)",
				agent.name, agent.cmd.ProcessState)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s agent did not enrol within %s", agent.name, twoAgentEnrolTimeout)
	return enrolment{}
}

// Two distinct agents, two suppliers, two engines, enrolling on their own.
func TestTwoDistinctAgentsEnrolAutonomously(t *testing.T) {
	agentBinaryPath(t) // skip early when there is no binary to run
	llamaURL := os.Getenv("MERC_LLAMA_EMBED_URL")
	if llamaURL == "" {
		t.Skip("MERC_LLAMA_EMBED_URL is not set; the llama.cpp agent has no engine to reach")
	}

	ctx, store, pool := openIsolatedMoneyPathStore(t)
	// The production router, not a bespoke mux: enrolment goes through the same
	// handlers, auth and validation a real worker meets.
	srv := httptest.NewServer(NewServer(store, nil, nil, nil).Routes())
	t.Cleanup(srv.Close)

	// Staggered: both agents benchmark on Metal at startup, and launching them
	// together makes each wait on the other's GPU work for nothing.
	candle := launchAgent(t, ctx, store, pool, srv.URL, "candle", "candle_metal", llamaURL)
	candleEnrolment := waitForEnrolment(t, ctx, pool, candle)
	llama := launchAgent(t, ctx, store, pool, srv.URL, "llama_cpp", "llama_cpp_metal", llamaURL)
	llamaEnrolment := waitForEnrolment(t, ctx, pool, llama)

	if candle.workerID == llama.workerID || candle.supplierID == llama.supplierID {
		t.Fatal("the two agents share an identity; attribution bugs could hide in one balance")
	}

	for name, got := range map[string]enrolment{
		"candle": candleEnrolment, "llama_cpp": llamaEnrolment,
	} {
		t.Logf("%s enrolled: engine=%s profile=%s/%s digest=%s… cells=%v routable=%v",
			name, got.engine, got.profileID, got.revision, got.digest[:12],
			got.cells, got.routable)

		if len(got.digest) != 64 || got.revision == "" {
			t.Errorf("%s: incomplete governed identity %s/%s", name, got.revision, got.digest)
		}
		// The stored digest must be the AUTHORITY's, not something the worker
		// asserted: a worker that could name its own digest could claim to be any
		// revision.
		profile, ok := runtimeProfileByID(got.profileID)
		if !ok {
			t.Fatalf("%s: enrolled against unregistered profile %q", name, got.profileID)
		}
		want, err := profile.ContentDigest(runtimeAuthorityModels)
		if err != nil {
			t.Fatal(err)
		}
		if got.digest != want || got.revision != profile.Revision {
			t.Errorf("%s: stored %s/%s, authority says %s/%s",
				name, got.revision, got.digest, profile.Revision, want)
		}
	}

	// Each agent enrolled against its OWN engine's profile.
	if candleEnrolment.profileID != "candle_metal" {
		t.Errorf("candle agent bound %s", candleEnrolment.profileID)
	}
	if llamaEnrolment.profileID != "llama_cpp_metal" {
		t.Errorf("llama.cpp agent bound %s, want llama_cpp_metal", llamaEnrolment.profileID)
	}

	// The llama.cpp agent advertises the DIRECTED-ONLY embed cell, which is the
	// whole point: it can now be the worker that drives the chain proving it.
	if !containsCell(llamaEnrolment.cells, llamaEmbedCell) {
		t.Errorf("llama.cpp agent did not advertise %s: %v", llamaEmbedCell, llamaEnrolment.cells)
	}
	// And not the rejected generation cell, at any point.
	if containsCell(llamaEnrolment.cells, llamaInferCell) {
		t.Errorf("llama.cpp agent advertised the REJECTED_FOR_CONTRACT cell: %v",
			llamaEnrolment.cells)
	}
	if !containsCell(candleEnrolment.cells, candleEmbedCell) {
		t.Errorf("candle agent did not advertise %s: %v", candleEmbedCell, candleEnrolment.cells)
	}

	// Routability is recorded from the advertised projection, never from the
	// worker. This is what keeps the enrolment widening from widening the product.
	for _, routable := range llamaEnrolment.routable {
		if routable {
			t.Error("the llama.cpp agent holds a ROUTABLE capability; a directed-only " +
				"cell must not be reachable by ordinary buyer work")
		}
	}
	for _, routable := range candleEnrolment.routable {
		if !routable {
			t.Error("a candle capability is non-routable; the active production " +
				"runtime would stop receiving work")
		}
	}
}

func containsCell(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// A directed job reaches the intended agent and no other.
//
// This is the routing half of the autonomous claim: two jobs, one frozen onto
// each embed cell, offered to the real capabilities two real agents advertised.
// The claim runs against those stored rows rather than against fixtures, so it
// exercises the same predicate a live poll does.
func TestDirectedJobsReachOnlyTheIntendedAgent(t *testing.T) {
	agentBinaryPath(t)
	llamaURL := os.Getenv("MERC_LLAMA_EMBED_URL")
	if llamaURL == "" {
		t.Skip("MERC_LLAMA_EMBED_URL is not set; the llama.cpp agent has no engine to reach")
	}

	ctx, store, pool := openIsolatedMoneyPathStore(t)
	srv := httptest.NewServer(NewServer(store, nil, nil, nil).Routes())
	t.Cleanup(srv.Close)

	candle := launchAgent(t, ctx, store, pool, srv.URL, "candle", "candle_metal", llamaURL)
	waitForEnrolment(t, ctx, pool, candle)
	llama := launchAgent(t, ctx, store, pool, srv.URL, "llama_cpp", "llama_cpp_metal", llamaURL)
	waitForEnrolment(t, ctx, pool, llama)

	// Stop the agents polling before the claim assertions: an agent that claims
	// the task first would make "the wrong agent cannot claim" unfalsifiable.
	for _, a := range []*enrolledAgent{candle, llama} {
		if a.cmd.Process != nil {
			_ = a.cmd.Process.Signal(os.Interrupt)
		}
	}
	time.Sleep(2 * time.Second)

	// A fresh deadline. The store context was budgeted for a test that does not
	// spend two minutes waiting on model benchmarks, and it expires mid-fixture
	// otherwise — reported as "create buyer: context deadline exceeded", which
	// reads like a database fault and is not one.
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)

	for _, tc := range []struct {
		name, cell string
		intended   *enrolledAgent
		other      *enrolledAgent
	}{
		{"candle cell", candleEmbedCell, candle, llama},
		{"llama.cpp cell", llamaEmbedCell, llama, candle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{TaskCount: 1})
			tasks := makeTasks(f, 1)
			f.TaskIDs = []uuid.UUID{tasks[0].ID}
			job := validJobRowDirected(t, f, tasks, tc.cell)
			if err := store.SubmitJobTx(ctx, job, tasks); err != nil {
				t.Fatalf("submit directed job: %v", err)
			}

			// The agent that does NOT hold this cell must not claim it, whatever
			// else it is capable of.
			wrong, err := store.ClaimTasksTx(ctx, WorkerAuth{
				WorkerID: tc.other.workerID, SupplierID: tc.other.supplierID,
			})
			if err != nil {
				t.Fatalf("wrong-agent claim: %v", err)
			}
			if wrong != nil {
				t.Fatalf("%s claimed a job directed to %s", tc.other.name, tc.cell)
			}

			// The agent that does must.
			right, err := store.ClaimTasksTx(ctx, WorkerAuth{
				WorkerID: tc.intended.workerID, SupplierID: tc.intended.supplierID,
			})
			if err != nil {
				t.Fatalf("intended-agent claim: %v", err)
			}
			if right == nil {
				t.Fatalf("%s did not receive the job directed to its own cell %s",
					tc.intended.name, tc.cell)
			}
			if right.JobID != f.JobID {
				t.Fatalf("claimed job %s, want %s", right.JobID, f.JobID)
			}
			t.Logf("%s claimed task %s on %s", tc.intended.name, right.TaskID, tc.cell)
		})
	}
}
