# Governed perception learning

The perception workspace can seed active learning without accepting model claims
as truth. `active_learning.start_from_workspace` converts persisted
contradictions and missing-observation findings into uncertainty/high-impact
predictions whose evidence citations remain attached. The existing transaction
then:

1. ranks a bounded correction queue;
2. requires named corrections against requested prediction IDs;
3. creates an immutable correction dataset;
4. plans offline training with network access disabled;
5. evaluates baseline and candidate predictions over the complete same fixed
   benchmark;
6. recomputes supported metrics from stored prediction artifacts;
7. activates only a commercially eligible checkpoint with at least one
   improvement and no regression after named review.

Caller-supplied before/after scores are rejected. Equal, incomplete, unbound,
regressing, tampered, or non-commercial candidates cannot activate.

Activation atomically supersedes the prior revision and records an artifact-bound
receipt. `active_learning.rollback` requires another named review, atomically
marks the current revision `ROLLED_BACK`, restores its predecessor when one
exists, and stores an auditable rollback receipt. Cycles, transitions,
activations, supersessions, and rollbacks remain preserved and are independently
checked by the project acceptance receipt.
