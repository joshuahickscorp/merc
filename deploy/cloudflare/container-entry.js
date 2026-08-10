// Routes requests to the merc control-plane container.
//
// This Worker is a front door, not logic. Every decision — pricing, admission,
// market clearing, settlement — stays inside the Go binary, because moving any
// of it here would create a second authority for something the control plane
// already owns, which is the defect class this programme most guards against.
import { Container, getContainer } from "@cloudflare/containers";

export class MercControl extends Container {
  defaultPort = 8080;
  // The control plane holds Postgres connections and background tickers, so a
  // short idle timeout would kill sweeps mid-flight. Ten minutes is long enough
  // that lease recovery and the verification processor survive a quiet period.
  sleepAfter = "10m";

  onStart() {
    console.log("merc control container started");
  }
  onError(err) {
    // Surface rather than swallow: a container that dies silently looks like a
    // network fault to every caller.
    console.error("merc control container error", err);
    throw err;
  }
}

export default {
  async fetch(request, env) {
    // Single instance by default. Sharding by buyer would need the same
    // per-buyer serialization the advisory lock provides, and that decision
    // belongs with the money path, not with routing.
    return getContainer(env.MERC_CONTROL).fetch(request);
  },
};
