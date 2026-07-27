# Ocular proposal-fusion builder inputs

This tree is the only builder-visible packet for the proposal ensemble.

- Public conditions and frozen thresholds live in `manifest.json` (parent) and
  `public_conditions.json`.
- **Never** open `../hidden/` from the builder path. That directory holds the
  sealed canary and hidden labels for the evaluator only.
- Thresholds are digest-bound in the parent manifest. Changing them after the
  hidden run starts is a contract violation.
