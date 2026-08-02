# Merc media-transcode contract

`ffmpeg-transcode-v1` is a fixed built-in runtime contract, not a buyer command
surface. The agent invokes the pinned local FFmpeg/ffprobe executables with one
constant video-only template, clears the child environment, strips metadata,
and verifies the output before it can be committed.

The control plane accepts only MP4, MOV, WebM or Matroska inputs up to 64 MiB.
Width and height are even values from 64 through 4096, frame rate is 1–60 FPS,
and the requested H.264 bitrate is 200–50,000 kbps. The resulting MP4 is capped
at 64 MiB by the control-plane verification policy; the agent's stricter
runtime contract also bounds dimensions, duration, frame rate and process time.

This document is the identity contract for the built-in model row. It does not
grant rights to FFmpeg, libx264, or any other third-party codec. Those runtime
and distribution terms remain a separate legal review item until the release
image's exact binaries and notices are verified.
