# Closed-loop frontend repair

`vision.compare` evaluates compatible `LayoutGraph` captures by stable selector. It reports
component-local position/size residuals, computed-style differences, semantic/accessibility
differences, missing nodes, optional screenshot RMS, exact graph citations, weighted score, and
independent acceptance gates.

`vision.repair(action="portfolio")` evaluates a bounded set of already captured candidate
parameters inside an explicit selector locality. Every attempt is stored and ranked. The selected
local candidate must then pass `vision.evaluate(portfolio_id=...)` or
`vision.repair(action="global_gate")`, which reruns the full graph comparison atomically. A fixture
proves that a perfect hero repair with an injected footer regression wins the local search and is
then rejected by the mandatory global gate; the failed candidate remains reproducible.

For owned project files, `vision.repair(action="propose_css")` produces a narrow CSS override from
the implicated node residuals and stores both the patch plan and exact resulting file as immutable
artifacts. It cannot write until `action="review"` records a named approval and reason. Application
rechecks the exact base digest, refuses concurrent edits and path escape, preserves the original as
a backup artifact, writes atomically, and emits an application receipt.

This service repairs frontend CSS evidence. Existing VisionMCP scene repair continues to govern
Blender proposals, checkpoints, named review, and final acceptance. Neither path promotes a local
improvement without its required global gates.
