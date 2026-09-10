# Third-party components

| component | version | license | role |
| --- | --- | --- | --- |
| vidc (this repo's own code) | — | MIT ([LICENSE](LICENSE)) | the app |
| ffmpeg | 9.0.1 | GPL-2.0+ (`third_party/ffmpeg/COPYING.GPL`) | bundled encoder/muxer/prober, invoked as a separate process |
| x264 | b35605a | GPL-2.0+ | H.264 encoder, statically linked into the bundled ffmpeg |
| libvmaf | v3.2.0 | BSD-2-Clause-Patent | quality metric, statically linked into the bundled ffmpeg |
| github.com/crgimenes/glaze | v0.0.54 | MIT | app window (WebView) |
| github.com/ebitengine/purego | v0.10.2 | BSD-2-Clause | glaze's CGo-free FFI (indirect) |
| golang.org/x/term | v0.46.0 | BSD-3-Clause | terminal detection |
| golang.org/x/sys | v0.48.0 | BSD-3-Clause | x/term's syscall layer (indirect) |

The ffmpeg build recipe is `build/ffmpeg-build.sh`; see `third_party/ffmpeg/BUILD.md`
for the bundled-component rationale, exact configure line, and source locations.
