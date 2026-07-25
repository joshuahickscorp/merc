# Blender extension

The Blender 4.2+ extension is a thin operator surface for an external `bvmcp` installation. It
selects portable project directories and project-confined artifacts, submits allow-listed workflow
jobs, polls coordinator status, requests cancellation, and launches the loopback review dashboard.

It intentionally owns no socket listener, model backend, database, or reconstruction algorithm.
All expensive and security-sensitive work remains in the coordinator and isolated Blender worker.

Validate and package it with Blender:

```bash
/Applications/Blender.app/Contents/MacOS/Blender --command extension validate blender_extension
/Applications/Blender.app/Contents/MacOS/Blender --command extension build \
  --source-dir blender_extension --output-dir dist
```
