# Release-candidate dependency review

Status: source review complete for the supervised test-mode canary. This is
not a live-money dependency approval.

## Rust `paste` 1.0.15

`paste` is not a direct ComputExchange dependency and there is no `paste!`
use in the agent source. `cargo tree -i paste` shows it is required by the
locked numeric/runtime graph: Candle 0.10.2 through `gemm`/`pulp`, and both the
Candle and direct Tokenizers graphs through `macro_rules_attribute`.

Removing it would require replacing or forking the pinned model runtime, not a
small local macro rewrite. It is therefore retained for this RC. Locked Cargo
metadata reports `paste` 1.0.15 from crates.io, repository
`https://github.com/dtolnay/paste`, licensed `MIT OR Apache-2.0`. CI runs the
locked RustSec audit; the RC gate fails on an applicable advisory.

Re-evaluate this transitive edge whenever Candle or Tokenizers is upgraded.
