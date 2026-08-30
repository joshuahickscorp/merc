# Historical Network V2 authority migration digest

This compact record preserves the deletion rules behind the completed
authority migration. It is not an execution queue and cannot authorize a
release.

## 2. Migration boundaries

- Registration stores a sealed capability snapshot; worker rows and routable
  projections are derived views, not a second capability authority.
- Accepted placement, pricing, execution, verification, and settlement facts
  carry their own version and digest.
- Compatibility fields remain only where an older wire or database record
  must replay honestly. They are never refreshed from current policy.
- Shadow and measurement paths observe the canonical decision but cannot
  reserve capacity, write money state, or promote evidence.
- A deletion is complete only when callers, replay tests, migrations, and
  evidence bindings all point at the canonical authority.

The current source, schema, and bound receipts are the authority. Git history
retains the detailed migration register and its dated implementation notes.
