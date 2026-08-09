package main

import (
	"math"
	"net/http"
	"testing"
)

// The validation matrix is the part of job submission most likely to be edited
// and most likely to be wrong, and before the extraction it was reachable only
// through a ~500-line handler needing live Postgres and MinIO.  These cases run
// in microseconds against no dependencies.
func TestNormalizeAndValidateJobSubmit(t *testing.T) {
	// This suite exercises the request-only preflight used before durable
	// activation is loaded. Passing false is the production structural pass; it
	// keeps these assertions independent of the honest zero-lane production
	// authority census.
	normalize := func(in jobSubmit) (jobSubmit, *httpError) {
		return normalizeAndValidateJobSubmit(in, false)
	}
	valid := func() jobSubmit {
		return jobSubmit{
			JobType: JobType{Type: "embed"},
			Model:   ModelRef{Ref: "all-minilm-l6-v2"},
			Tier:    "batch",
		}
	}

	t.Run("defaults the tier", func(t *testing.T) {
		in := valid()
		in.Tier = ""
		out, herr := normalize(in)
		if herr != nil {
			t.Fatalf("unexpected error: %v", herr.msg)
		}
		if out.Tier != "batch" {
			t.Fatalf("tier = %q, want batch", out.Tier)
		}
	})

	t.Run("clamps an unbounded duration", func(t *testing.T) {
		in := valid()
		in.Constraints.MaxDurationSecs = 0
		out, herr := normalize(in)
		if herr != nil {
			t.Fatalf("unexpected error: %v", herr.msg)
		}
		if out.Constraints.MaxDurationSecs != defaultMaxJobDurationSecs {
			t.Fatalf("max_duration_secs = %d, want the platform ceiling %d",
				out.Constraints.MaxDurationSecs, defaultMaxJobDurationSecs)
		}
	})

	for name, tc := range map[string]struct {
		mutate func(*jobSubmit)
		status int
	}{
		"empty job type":      {func(s *jobSubmit) { s.JobType.Type = "" }, http.StatusBadRequest},
		"unknown job type":    {func(s *jobSubmit) { s.JobType.Type = "mine_bitcoin" }, http.StatusBadRequest},
		"unknown tier":        {func(s *jobSubmit) { s.Tier = "platinum" }, http.StatusBadRequest},
		"unknown hw class":    {func(s *jobSubmit) { s.Constraints.HWClasses = []string{"nvidia_h100"} }, http.StatusBadRequest},
		"nonzero temperature": {func(s *jobSubmit) { s.JobType.Temperature = 0.7 }, http.StatusBadRequest},
		"NaN max usd":         {func(s *jobSubmit) { s.MaxUSD = math.NaN() }, http.StatusBadRequest},
		"infinite max usd":    {func(s *jobSubmit) { s.MaxUSD = math.Inf(1) }, http.StatusBadRequest},
		"deadline too short":  {func(s *jobSubmit) { s.DeadlineSecs = 59 }, http.StatusBadRequest},
		"deadline too long":   {func(s *jobSubmit) { s.DeadlineSecs = 604801 }, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			in := valid()
			tc.mutate(&in)
			if _, herr := normalize(in); herr == nil {
				t.Fatal("invalid submission accepted")
			} else if herr.status != tc.status {
				t.Fatalf("status = %d, want %d (%s)", herr.status, tc.status, herr.msg)
			}
		})
	}

	t.Run("accepts the documented deadline sentinels", func(t *testing.T) {
		for _, secs := range []int{0, -1, 60, 604800} {
			in := valid()
			in.DeadlineSecs = secs
			if _, herr := normalize(in); herr != nil {
				t.Fatalf("deadline_secs=%d rejected: %s", secs, herr.msg)
			}
		}
	})
}
