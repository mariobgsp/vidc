# Bundled ffmpeg

vidc ships a minimal static ffmpeg build (plus the `ffprobe` produced by the same
build) so the app runs with no system ffmpeg installed. This directory records what
is bundled and how to rebuild it.

## What is bundled

- **ffmpeg 9.0.1** — muxing, decoding, filtering, probing
  (`https://ffmpeg.org/releases/ffmpeg-9.0.1.tar.xz`)
- **x264 b35605a** — the H.264 encoder
  (`https://code.videolan.org/videolan/x264.git`, branch `b35605a`)
- **libvmaf v3.2.0** — the quality metric
  (`https://github.com/Netflix/vmaf.git`, branch `v3.2.0`)

## Configure line

The exact invocation lives in `build/ffmpeg-build.sh` — that script is the committed
build recipe and is what CI runs. In short: `--disable-everything` plus only the
encoders (`libx264,aac,wrapped_avframe`), decoders, muxers (`mp4,mov,null`),
demuxers, parsers, filters (`setpts,transpose,format,scale,null,trim,atrim,aresample,
anull,settb,libvmaf`) and protocols (`file,pipe`) that vidc exercises, linked fully
statically.

Two details are load-bearing and must not be "cleaned up":

- `xxd` must be installed where the recipe runs. Meson uses it to embed the
  VMAF model JSON into libvmaf; without it the build still succeeds but the
  models are silently missing, and every VMAF measurement fails at runtime
  ("no such built-in model"). The recipe fails fast if `xxd` is absent, and
  CI asserts the embedded model with `strings ffmpeg | grep vmaf_v0.6.1`.

- `libvmaf` must appear in `--enable-filter`, not just `--enable-libvmaf`.
  `--disable-everything` disables all filters, so the library alone links without
  enabling the filter — configure still lists it under "External libraries" while the
  binary silently has no libvmaf filter (and vidc permanently reports "VMAF skipped").
- `wrapped_avframe` must appear in `--enable-encoder`. Without it every `-f null -`
  fails, which breaks both the VMAF measurement and `--size` 2-pass pass 1.

Corresponding source for any shipped binary can be obtained from the URLs above at
the pinned revisions, and rebuilt with `build/ffmpeg-build.sh`. The full GPL-2.0 text
is `COPYING.GPL` in this directory; pinned revisions are in `VERSION`.

## Licensing

vidc itself is MIT. It invokes the GPL ffmpeg/ffprobe binaries as **separate
processes** (mere aggregation, not linking). The ffmpeg side stays GPL: its source,
this build recipe, and the license text ship alongside as required.
