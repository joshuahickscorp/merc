package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/google/uuid"
)

type VerifyOutcome string

const (
	OutcomePass            VerifyOutcome = "pass"
	OutcomeFail            VerifyOutcome = "fail"
	OutcomePassWithPenalty VerifyOutcome = "pass_with_penalty"
	OutcomeLossNoPayout    VerifyOutcome = "loss_no_payout"
)

type Verifier struct {
	store        verificationStore
	storage      *Storage
	sampleSecret []byte
	planning     bool
}

const insecureDevelopmentSamplingSecret = "merc-insecure-development-sampling-secret"

// verificationSampleSecretFromEnv returns the sampling secret. The published
// development default is refused whenever any Stripe key is configured —
// including sk_test_ — because test money still buys fraud practice.
func verificationSampleSecretFromEnv() (string, error) {
	secret := os.Getenv("MERC_VERIFICATION_SAMPLE_SECRET")
	if secret == "" {
		secret = insecureDevelopmentSamplingSecret
	}
	if (secret == insecureDevelopmentSamplingSecret || len(secret) < 32) && stripeKey() != "" {
		return "", fmt.Errorf("MERC_VERIFICATION_SAMPLE_SECRET missing or is the published development default while STRIPE_SECRET_KEY is set; refusing unhardened verification sampling with money rails present")
	}
	return secret, nil
}

func NewVerifier(s *Store) *Verifier {
	secret, err := verificationSampleSecretFromEnv()
	if err != nil {
		// Fail closed: main() already refuses via validateHardeningSecretConfig.
		// Callers outside that gate (e.g. NewWorkers) must not construct a
		// verifier with the published default while money rails are present.
		panic(err.Error())
	}
	return &Verifier{store: s, sampleSecret: []byte(secret)}
}

func (v *Verifier) WithSamplingSecret(secret []byte) *Verifier {
	v.sampleSecret = append(v.sampleSecret[:0], secret...)
	return v
}

func (v *Verifier) WithStorage(st *Storage) *Verifier { v.storage = st; return v }

func (v *Verifier) verifyTaskResult(ctx context.Context, info *CommitTaskInfo, commit TaskCommit, commitBytes, redundancyBytes []byte) (VerifyOutcome, error) {
	var checkProbCached float64
	var checkProbDone bool
	checkProb := func() float64 {
		if !checkProbDone {
			checkProbCached = v.effectiveCheckProb(ctx, info)
			checkProbDone = true
		}
		return checkProbCached
	}
	checkSelected := func() bool {
		if info.verificationCheckSampled != nil {
			return *info.verificationCheckSampled
		}
		return v.checkSampled(info.TaskID, checkProb())
	}

	// Known-answer honeypot checks are always run: the answer is already stored
	// and the comparison is a byte compare. Sampling remains only for expensive
	// redundancy/tiebreak paths via checkSelected() below.
	if info.IsHoneypot {
		known, answerClass, err := v.store.GetHoneypotAnswer(ctx, jobTypeOf(info), info.InputRef)
		if err != nil && !errors.Is(err, errNotFound) {
			return OutcomeFail, err
		}
		if known == nil {
			// The task was dispatched as a probe but no known answer is stored for
			// it. That is a control-plane integrity failure, not supplier
			// misconduct, so no reputation dock and no quarantine -- but it must
			// not pay either, because the one check that distinguishes correct work
			// from shape-valid garbage did not run. createJob refuses to attach a
			// probe it cannot back, so reaching here means the alias was lost after
			// dispatch; record it loudly rather than falling through to a pass.
			if err := v.store.RecordVerificationEvent(ctx, info.JobID, info.TaskID, info.SupplierID, "honeypot_answer_missing"); err != nil {
				return OutcomeFail, err
			}
			return OutcomeFail, nil
		}
		{
			// Byte-exact honeypots with a stored answer_class fail closed on class
			// mismatch. engine/buildHash are worker-declared; skipping the compare
			// would let an unexpected build_hash disarm the probe.
			if byteExactJobType(info.jobType) && answerClass != "" &&
				answerClass != classKey(info.engine, info.buildHash) {
				if err := v.store.DockReputation(ctx, info.SupplierID, EventHoneypotFail); err != nil {
					return OutcomeFail, err
				}
				if err := v.store.RecordVerificationEvent(ctx, info.JobID, info.TaskID, info.SupplierID, "honeypot_class_mismatch"); err != nil {
					return OutcomeFail, err
				}
				if err := v.store.ClawbackTaskCredit(ctx, info.SupplierID, info.TaskID); err != nil {
					return OutcomeFail, err
				}
				if err := v.store.QuarantineSupplier(ctx, info.SupplierID); err != nil {
					return OutcomeFail, err
				}
				if err := v.store.RequeueTask(ctx, taskIDOf(commit)); err != nil {
					return OutcomeFail, err
				}
				return OutcomeFail, nil
			}
			// Class-blind paths: non-byte-exact job types always compare; byte-exact
			// honeypots seeded with blank answerClass keep intentional skip behaviour.
			if byteHoneypotComparable(info.jobType, answerClass, info.engine, info.buildHash) {
				if !resultsAgree(info.jobType, commitBytes, known) {
					if err := v.store.DockReputation(ctx, info.SupplierID, EventHoneypotFail); err != nil {
						return OutcomeFail, err
					}
					if err := v.store.RecordVerificationEvent(ctx, info.JobID, info.TaskID, info.SupplierID, "honeypot_fail"); err != nil {
						return OutcomeFail, err
					}
					if err := v.store.ClawbackTaskCredit(ctx, info.SupplierID, info.TaskID); err != nil {
						return OutcomeFail, err
					}
					if err := v.store.QuarantineSupplier(ctx, info.SupplierID); err != nil {
						return OutcomeFail, err
					}
					if err := v.store.RequeueTask(ctx, taskIDOf(commit)); err != nil {
						return OutcomeFail, err
					}
					return OutcomeFail, nil
				}
				if err := v.store.DockReputation(ctx, info.SupplierID, EventHoneypotPass); err != nil {
					return OutcomePass, err
				}
				if err := v.store.RecordVerificationEvent(ctx, info.JobID, info.TaskID, info.SupplierID, "honeypot_pass"); err != nil {
					return OutcomePass, err
				}
				return OutcomePass, nil
			}
		}
	}

	if redundancyBytes != nil {
		all, ferr := v.gatherChunkResults(ctx, info, commitBytes)
		if ferr != nil {
			return OutcomeFail, ferr
		}
		if len(all) == 0 {
			all = []chunkVote{
				{supplierID: info.SupplierID, taskID: info.TaskID, bytes: commitBytes, engine: info.engine, buildHash: info.buildHash},
				{supplierID: info.peerSupplierID, bytes: redundancyBytes, engine: info.peerEngine, buildHash: info.peerBuildHash},
			}
		}
		rawVotes := all
		all = independentSupplierVotes(all)
		switch {
		case len(all) >= 3:
			return v.resolveTiebreak(ctx, info, all)
		case len(all) == 2 && !resultsAgree(info.jobType, all[0].bytes, all[1].bytes):
			if byteExactJobType(info.jobType) &&
				!sameVerificationClass(all[0].engine, all[0].buildHash, all[1].engine, all[1].buildHash) {
				if err := v.store.RecordVerificationEvent(ctx, info.JobID, info.TaskID, info.SupplierID, "redundancy_cross_class"); err != nil {
					return OutcomePassWithPenalty, err
				}
				return OutcomePassWithPenalty, nil
			}
			if err := v.store.RecordVerificationEvent(ctx, info.JobID, info.TaskID, info.SupplierID, "redundancy_mismatch"); err != nil {
				return OutcomePassWithPenalty, err
			}
			if checkSelected() {
				if err := v.dispatchTiebreak(ctx, info, all); err != nil {
					return OutcomePassWithPenalty, err
				}
			}
			return OutcomePassWithPenalty, nil
		default:
			if independentRedundancyMatch(rawVotes) {
				if err := v.store.DockReputation(ctx, info.SupplierID, EventRedundancyMatch); err != nil {
					return OutcomePass, err
				}
				if err := v.store.RecordVerificationEvent(ctx, info.JobID, info.TaskID, info.SupplierID, "redundancy_match"); err != nil {
					return OutcomePass, err
				}
			} else {
				if err := v.store.RecordVerificationEvent(ctx, info.JobID, info.TaskID, info.SupplierID, "redundancy_same_supplier"); err != nil {
					return OutcomePass, err
				}
			}
		}
	} else if info.IsRedundancy {
		if err := v.store.DockReputation(ctx, info.SupplierID, EventTaskSuccess); err != nil {
			return OutcomePassWithPenalty, err
		}
		return OutcomePassWithPenalty, nil
	}

	if err := v.store.DockReputation(ctx, info.SupplierID, EventTaskSuccess); err != nil {
		return OutcomePass, err
	}
	return OutcomePass, nil
}

func independentRedundancyMatch(all []chunkVote) bool {
	if len(all) <= 1 {
		return true
	}
	return len(independentSupplierVotes(all)) > 1
}

const (
	verifyTrustFloor     = 0.90
	verifyCheckProbFloor = 0.25
)

func (v *Verifier) effectiveCheckProb(ctx context.Context, info *CommitTaskInfo) float64 {
	// A governed class is not a coin flip.
	//
	// Expressed as a probability of 1.0 rather than as a branch around the
	// sampling machinery, so the pinned sampling row still records what actually
	// decided this task — probability 1, selected true — instead of recording a
	// reputation-derived number that had no bearing on the outcome. The class
	// itself is stored beside it, so a reader can tell "certain because governed"
	// from "certain because the supplier is new".
	if verificationClassAlwaysChecked(info.VerificationClass) {
		return 1.0
	}
	rep, ok := v.committingReputation(ctx, info)
	if !ok || rep <= verifyTrustFloor {
		return 1.0
	}
	if rep > 1.0 {
		rep = 1.0
	}
	frac := float64(rep-verifyTrustFloor) / float64(1.0-verifyTrustFloor)
	prob := 1.0 - frac*(1.0-verifyCheckProbFloor)
	if prob < verifyCheckProbFloor {
		prob = verifyCheckProbFloor
	}
	return prob
}

func (v *Verifier) committingReputation(ctx context.Context, info *CommitTaskInfo) (float32, bool) {
	if info.WorkerID == uuid.Nil {
		return 0, false
	}
	cands, err := v.store.CandidateWorkers(ctx, info.jobType, info.ModelRef, info.MinMemoryGB)
	if err != nil {
		return 0, false // a read failure must not silently lower the check rate
	}
	for _, c := range cands {
		if c.ID == info.WorkerID {
			return c.Reputation, true
		}
	}
	return 0, false
}

func (v *Verifier) checkSampled(taskID uuid.UUID, prob float64) bool {
	if prob >= 1.0 || prob <= 0 {
		return true
	}
	return taskSample(v.sampleSecret, taskID) < prob
}

func taskSample(secret []byte, taskID uuid.UUID) float64 {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("cx/verification-sample/v1\x00"))
	_, _ = mac.Write(taskID[:])
	sum := mac.Sum(nil)
	hi := binary.BigEndian.Uint64(sum[:8])
	return float64(hi) / float64(1<<64)
}

type chunkVote struct {
	supplierID uuid.UUID
	taskID     uuid.UUID
	bytes      []byte
	engine     string
	buildHash  string
}

func independentSupplierVotes(all []chunkVote) []chunkVote {
	if len(all) <= 1 {
		return append([]chunkVote(nil), all...)
	}
	representatives := make(map[uuid.UUID]chunkVote, len(all))
	for _, vote := range all {
		current, exists := representatives[vote.supplierID]
		if !exists || chunkVoteLess(vote, current) {
			representatives[vote.supplierID] = vote
		}
	}
	out := make([]chunkVote, 0, len(representatives))
	for _, vote := range representatives {
		out = append(out, vote)
	}
	sort.Slice(out, func(i, j int) bool { return chunkVoteLess(out[i], out[j]) })
	return out
}

func chunkVoteLess(a, b chunkVote) bool {
	if cmp := bytes.Compare(a.taskID[:], b.taskID[:]); cmp != 0 {
		return cmp < 0
	}
	if cmp := bytes.Compare(a.supplierID[:], b.supplierID[:]); cmp != 0 {
		return cmp < 0
	}
	if cmp := bytes.Compare(a.bytes, b.bytes); cmp != 0 {
		return cmp < 0
	}
	if a.engine != b.engine {
		return a.engine < b.engine
	}
	return a.buildHash < b.buildHash
}

func sameVerificationClass(aEngine, aBuild, bEngine, bBuild string) bool {
	if aBuild == "" || bBuild == "" {
		return false
	}
	return aEngine == bEngine && aBuild == bBuild
}

func classKey(engine, buildHash string) string {
	if buildHash == "" {
		return ""
	}
	return engine + "|" + buildHash
}

func byteExactJobType(jobType string) bool {
	return jobType != "embed"
}

func byteHoneypotComparable(jobType, answerClass, engine, buildHash string) bool {
	if !byteExactJobType(jobType) {
		return true
	}
	return answerClass != "" && answerClass == classKey(engine, buildHash)
}

var errHoneypotBlankClass = fmt.Errorf("byte-exact honeypot requires a non-blank answer_class")

func validateHoneypotSeed(jobType, answerClass string) error {
	if byteExactJobType(jobType) && answerClass == "" {
		return fmt.Errorf("%w: job_type=%s", errHoneypotBlankClass, jobType)
	}
	return nil
}

func (v *Verifier) gatherChunkResults(ctx context.Context, info *CommitTaskInfo, commitBytes []byte) ([]chunkVote, error) {
	if v.storage == nil || info.JobID == uuid.Nil {
		return nil, nil
	}
	rows, err := v.store.ChunkResults(ctx, info.JobID, info.ChunkIndex)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	readLimit := info.resultMaxBytes
	if readLimit <= 0 || readLimit > verificationArtifactAbsoluteMaxBytes {
		readLimit = verificationArtifactAbsoluteMaxBytes
	}
	out := make([]chunkVote, 0, len(rows))
	foundCommitting := false
	for _, cr := range rows {
		var b []byte
		if cr.TaskID == info.TaskID {
			foundCommitting = true
			b = commitBytes // already in hand; avoid a redundant fetch
		} else if cr.Artifact != nil {
			if byteExactJobType(info.jobType) && cr.Artifact.SHA256 == info.ResultSHA256 &&
				sameVerificationClass(info.engine, info.buildHash, cr.Engine, cr.BuildHash) {
				b = commitBytes
			} else {
				b, err = v.storage.readSealedVerificationArtifactWithLimit(ctx, *cr.Artifact, readLimit)
			}
			if err != nil {
				return nil, fmt.Errorf("read sealed vote artifact for task %s: %w", cr.TaskID, err)
			}
		} else {
			fetched, gerr := v.storage.readVerificationObjectBounded(ctx, cr.ResultRef, readLimit, nil)
			if gerr != nil {
				return nil, gerr
			}
			b = fetched
		}
		out = append(out, chunkVote{
			supplierID: cr.SupplierID,
			taskID:     cr.TaskID,
			bytes:      b,
			engine:     cr.Engine,
			buildHash:  cr.BuildHash,
		})
	}
	if !foundCommitting {
		out = append(out, chunkVote{
			supplierID: info.SupplierID,
			taskID:     info.TaskID,
			bytes:      commitBytes,
			engine:     info.engine,
			buildHash:  info.buildHash,
		})
	}
	return out, nil
}

func (v *Verifier) resolveTiebreak(ctx context.Context, info *CommitTaskInfo, all []chunkVote) (VerifyOutcome, error) {
	all = independentSupplierVotes(all)
	blobs := make([][]byte, len(all))
	for i, c := range all {
		blobs[i] = c.bytes
	}
	winner := majorityVote(info.jobType, blobs)
	agreeWinner := 0
	for _, c := range all {
		if resultsAgree(info.jobType, c.bytes, winner) {
			agreeWinner++
		}
	}
	if agreeWinner*2 <= len(all) {
		return OutcomePassWithPenalty, nil
	}
	byteExact := byteExactJobType(info.jobType)
	var winEngine, winBuild string
	for _, c := range all {
		if resultsAgree(info.jobType, c.bytes, winner) {
			winEngine, winBuild = c.engine, c.buildHash
			break
		}
	}
	mismatch := false
	committerLost := false
	for _, c := range all {
		switch {
		case resultsAgree(info.jobType, c.bytes, winner):
			if err := v.store.DockReputation(ctx, c.supplierID, EventRedundancyMatch); err != nil {
				return OutcomeFail, err
			}
			if err := v.store.RecordVerificationEvent(ctx, info.JobID, c.taskID, c.supplierID, "tiebreak_win"); err != nil {
				return OutcomeFail, err
			}
		case byteExact && !sameVerificationClass(winEngine, winBuild, c.engine, c.buildHash):
			if err := v.store.DockReputation(ctx, c.supplierID, EventRedundancyMatch); err != nil {
				return OutcomeFail, err
			}
			if err := v.store.RecordVerificationEvent(ctx, info.JobID, c.taskID, c.supplierID, "tiebreak_cross_class"); err != nil {
				return OutcomeFail, err
			}
		default:
			mismatch = true
			if c.supplierID == info.SupplierID {
				committerLost = true
			}
			if err := v.store.DockReputation(ctx, c.supplierID, EventMismatch); err != nil {
				return OutcomeFail, err
			}
			if err := v.store.RecordVerificationEvent(ctx, info.JobID, c.taskID, c.supplierID, "tiebreak_loss"); err != nil {
				return OutcomeFail, err
			}
			if err := v.store.ClawbackTaskCredit(ctx, c.supplierID, c.taskID); err != nil {
				return OutcomeFail, err
			}
		}
	}
	if committerLost {
		return OutcomeLossNoPayout, nil
	}
	if mismatch {
		return OutcomePass, nil
	}
	return OutcomePass, nil
}

func (v *Verifier) dispatchTiebreak(ctx context.Context, info *CommitTaskInfo, all []chunkVote) error {
	if v.storage == nil {
		return nil // no full lifecycle available (unit context)
	}
	exists, err := v.store.TiebreakExists(ctx, info.JobID, info.ChunkIndex)
	if err != nil || exists {
		return err
	}
	chunk, err := v.store.ChunkResults(ctx, info.JobID, info.ChunkIndex)
	if err != nil {
		return err
	}
	var also []uuid.UUID
	var alsoSuppliers []uuid.UUID
	for _, cr := range chunk {
		if cr.WorkerID != info.WorkerID {
			also = append(also, cr.WorkerID)
			alsoSuppliers = append(alsoSuppliers, cr.SupplierID)
		}
	}
	peer, err := v.store.SelectRedundancyPeerExcluding(ctx, info.jobType, info.ModelRef, info.MinMemoryGB, info.WorkerID, also, alsoSuppliers)
	if errors.Is(err, ErrNoSupply) {
		return nil // no third same-class worker online  -  provisional trust, logged upstream
	}
	if err != nil {
		return err
	}
	if _, err := v.store.InsertTiebreakTask(ctx, info.JobID, info.TaskID, peer, info.InputRef, info.ChunkIndex); err != nil {
		return err
	}
	if !v.planning {
		metrics.tiebreaks.Add(1)
	}
	return nil
}

// resultsAgree decides whether an observed artifact satisfies the equivalence
// contract its cell sells.
//
// The embed arm delegates to the governed comparator (embedding_comparator.go)
// rather than testing a mean here. A mean alone conceals a localized error: one
// zeroed row among 999 perfect ones averages to exactly 0.999 and used to pass
// this threshold. The comparator checks cardinality, per-row dimensions, finite
// values, nonzero norms and a per-row cosine floor, and records the failing row.
//
// `a` is the governed reference and `b` the candidate. Every caller passes the
// known/winning answer first.
func resultsAgree(jobType string, a, b []byte) bool {
	switch jobType {
	case "embed":
		return CompareEmbeddings(a, b).Passed
	case "batch_infer":
		return bytes.Equal(a, b)
	case "media_transcode":
		return bytes.Equal(a, b)
	case "media_rendering":
		return bytes.Equal(a, b)
	case "video_generation":
		// Bound to one pinned engine build on one hardware class. There is no
		// perceptual threshold; cross-supplier disagreement is refused at the
		// redundancy product, not papered over here.
		return bytes.Equal(a, b)
	default:
		return false
	}
}

func parseEmbeddingVectors(obj []byte) (vectors [][]float64, ok bool) {
	if isEmbedBinary(obj) {
		envelope, err := parseEmbeddingBinaryEnvelope(obj, 0)
		if err != nil {
			return nil, false
		}
		dim, count := int(envelope.Dim), int(envelope.Count)
		rows := make([][]float64, count)
		off := 0
		for r := 0; r < count; r++ {
			row := make([]float64, dim)
			for c := 0; c < dim; c++ {
				row[c] = float64(math.Float32frombits(binary.LittleEndian.Uint32(envelope.Body[off : off+4])))
				off += 4
			}
			rows[r] = row
		}
		return rows, true
	}
	rows, err := parseEmbeddingJSONVectors(obj, 0, 0, false)
	if err != nil {
		return nil, false
	}
	return rows, true
}

// meanCosine is deleted. It was the whole of the old embed contract, and leaving
// it beside the comparator would leave a second, laxer equivalence authority for
// someone to call by accident.

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func majorityVote(jobType string, results [][]byte) []byte {
	if len(results) == 0 {
		return nil
	}
	for i := range results {
		votes := 0
		for j := range results {
			if resultsAgree(jobType, results[i], results[j]) {
				votes++
			}
		}
		if votes*2 > len(results) {
			return results[i]
		}
	}
	return results[0] // no majority: fall back to the first (primary) result
}

func jobTypeOf(info *CommitTaskInfo) string { return info.jobType }

func taskIDOf(c TaskCommit) uuid.UUID { return c.TaskID }
