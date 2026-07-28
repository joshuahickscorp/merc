# Local review application

Run `bvmcp review serve --project PATH --open` to open the V1 review dashboard. The application
consumes coordinator resources for comparison, evidence, coverage, component, repair, job, and
acceptance views. It never talks to Blender directly; checkpoint work is queued through the same
governed coordinator used by CLI and MCP clients.
