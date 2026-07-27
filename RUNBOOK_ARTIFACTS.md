# Artifact runbook

Status as of 2026-07-21: the batch lane has S3-compatible presigned input/output
transfers, checksums, bounded retries, partial-result support, and governed model
pins. A complete buyer-facing artifact control plane and a real vLLM cache load
are not proven.

## Batch artifact incident checks

1. Identify the project, job, task, object key, expected SHA-256, byte bound,
   media type, and presigned-URL expiry without logging URL credentials.
2. Check object-store health and whether the failure is upload, download,
   expiry, range-resume, checksum, size, or authorization related.
3. Keep failed verification-staging objects isolated. Do not promote an object
   whose exact bytes, size, and digest do not match the fenced work record.
4. Reissue only task-scoped, short-lived credentials. Never grant a worker
   bucket-wide credentials or a cross-project object prefix.
5. Preserve the authoritative content-addressed record and receipt evidence
   before retention or deletion actions.

## Realtime model-cache checks

The vLLM supervisor mounts the operator-selected cache directory into the
digest-pinned container and passes exact model and tokenizer revisions. On a
real worker, verify the host architecture, container manifest, downloaded file
digests, cache ownership and permissions, available capacity, load duration,
and post-load health before advertising the offer as ready. A successful
software command-construction test is not proof that model acquisition or CUDA
loading succeeded.

## Required next proof

Implement project-namespaced initiate/multipart/complete/abort upload, presigned
download, metadata, retention, deletion, and artifact references without
proxying large bytes through the control API. Exercise cache miss, corruption,
interrupted download, resume, eviction, expiry, and tenant-isolation failures.

| Layer | Status |
|---|---|
| Implemented | Batch presigned transfer path; vLLM cache mount contract |
| Tested | Existing batch S3 retry/range/checksum tests; supervisor command test |
| Real-runtime proven | Batch evidence only; realtime model cache no |
| Private-canary proven | No |
| Production proven | No |
| Externally blocked | Object-store deployment for new API and Linux CUDA worker |
