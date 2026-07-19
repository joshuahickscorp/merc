package core

import "testing"

func TestReducerLegalAndIllegalTransitions(t *testing.T) {
	legal := []struct {
		e        Entity
		from, to string
	}{
		{EntityJob, "queued", "running"},
		{EntityJob, "running", "verifying"},
		{EntityJob, "verifying", "complete"},
		{EntityTask, "queued", "leased"},
		{EntityTask, "leased", "queued"}, // lease expiry requeue
		{EntityTask, "verifying", "done"},
		{EntityPayout, "held", "released"},
		{EntityPayout, "held", "clawed_back"},
		{EntityPayout, "released", "reversal_required"},
		{EntityCharge, "not_attempted", "attempting"},
		{EntityCharge, "attempting", "charged"},
		{EntityVerification, "pending", "terminal"},
	}
	for _, c := range legal {
		if !Legal(c.e, c.from, c.to) {
			t.Errorf("expected legal %s: %s -> %s", c.e, c.from, c.to)
		}
		if _, f := Reduce(c.e, c.from, c.to); f != nil {
			t.Errorf("Reduce refused legal %s: %s -> %s: %v", c.e, c.from, c.to, f)
		}
	}

	illegal := []struct {
		e        Entity
		from, to string
	}{
		{EntityJob, "complete", "running"},        // no resurrection
		{EntityJob, "queued", "complete"},         // must pass through running/verifying
		{EntityPayout, "clawed_back", "released"}, // clawed back is terminal
		{EntityPayout, "released", "held"},        // no un-release
		{EntityCharge, "charged", "attempting"},   // charged is terminal
		{EntityTask, "done", "running"},           // done is terminal
		{EntityVerification, "terminal", "leased"},
		{"nonsense", "a", "b"},
	}
	for _, c := range illegal {
		if Legal(c.e, c.from, c.to) {
			t.Errorf("expected ILLEGAL %s: %s -> %s", c.e, c.from, c.to)
		}
		if _, f := Reduce(c.e, c.from, c.to); f == nil {
			t.Errorf("Reduce accepted illegal %s: %s -> %s", c.e, c.from, c.to)
		}
	}
}

func TestTerminalStates(t *testing.T) {
	for _, s := range []string{"complete", "failed", "cancelled"} {
		if !Terminal(EntityJob, s) {
			t.Errorf("job %s should be terminal", s)
		}
	}
	if Terminal(EntityJob, "running") {
		t.Errorf("job running is not terminal")
	}
	if !Terminal(EntityPayout, "reversed") || !Terminal(EntityPayout, "clawed_back") {
		t.Errorf("reversed/clawed_back payouts are terminal")
	}
}

func TestWorkloadRegistry(t *testing.T) {
	for _, name := range []string{"embed", "batch_infer", "classify", "rerank", "json_extraction", "audio_transcribe"} {
		w, ok := Lookup(name)
		if !ok {
			t.Fatalf("workload %q must be registered", name)
		}
		if w.Name != name {
			t.Errorf("workload %q has Name=%q", name, w.Name)
		}
		if !w.ModelRequired {
			t.Errorf("workload %q must require a model", name)
		}
	}
	if _, ok := Lookup("does_not_exist"); ok {
		t.Errorf("unknown workload must fail closed")
	}
	// generative workloads route the token path; the flag is the single authority.
	if w, _ := Lookup("batch_infer"); !w.Generative {
		t.Errorf("batch_infer is generative")
	}
	if w, _ := Lookup("embed"); w.Generative {
		t.Errorf("embed is not generative")
	}
	if w, _ := Lookup("audio_transcribe"); w.Input != InputAudio || w.Split != SplitWhole {
		t.Errorf("audio_transcribe is a whole-input audio workload")
	}
}
