# Merc video-generation contract

`merc-video-synth-v1` is a Merc-owned deterministic synthesizer used only to
exercise the governed `video_generation` lane. It is not a diffusion weight set,
not an open video model, and not buyer-routable.

## What this lane is

A video job is N ordered time segments covering a requested duration. Segment
ordinals are the existing media ChunkIndex bijection. Coverage, duration-sum
and cross-supplier redundancy rules are the settlement gates. Verification is
byte-exact under one pinned engine build on one hardware class.

## What this lane is not

There is no pinnable open-licensed video weight set in-tree. This contract does
not authorize public payment, buyer advertisement, or any claim that a
prompt-to-video diffusion model is in service. Cross-supplier determinism is
refused rather than approximated with a perceptual threshold.
