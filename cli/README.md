# ComputeExchange CLI

`cx` is the buyer and operator command-line client for ComputeExchange.

Set `CX_CONTROL_URL` to the control-plane HTTPS origin and `CX_API_KEY` to a
revocable buyer key. Run `./cx help` for commands. Job submissions require an
idempotency key; the CLI creates one automatically, and `--idempotency-key`
allows an uncertain request to be retried with the same key and identical body.

Verify the archive against the adjacent `SHA256SUMS` file before installing.
Place `cx` on `PATH`; it does not require the source checkout or Go runtime.

Never put API keys in shell history, source control, or the archive directory.
