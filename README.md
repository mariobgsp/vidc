# vidc

A Go CLI that compresses videos into universally playable H.264 MP4s using ffmpeg, with a VMAF quality report.

## What it does

- Re-encodes to H.264 High profile + `yuv420p` + AAC in `.mp4`, which plays on essentially every phone, browser, TV, and chat app.
- Targets a *visually lossless* quality level (tuned CRF), so you normally cannot tell the difference — not bit-identical, and not a guarantee.
- Measures VMAF on a 30-second sample, scoring every 4th frame across all cores, and reports the score.
- Never modifies your source file. Output is written beside the original.
- Optionally hits an exact target file size (2-pass) for platform upload limits.
- Single static binary, no runtime dependencies beyond ffmpeg.

## Requirements

1. **ffmpeg and ffprobe.** vidc shells out to them; they must be on `PATH`.
   - Debian/Ubuntu: `sudo apt install ffmpeg`
   - Fedora: `sudo dnf install ffmpeg`
   - Arch: `sudo pacman -S ffmpeg`
   - macOS: `brew install ffmpeg`
   - Windows: `winget install Gyan.FFmpeg`
   - Otherwise: <https://ffmpeg.org/download.html>

   vidc checks for both on startup and prints the right hint for your OS if either is missing.

2. **Go 1.22+** — only needed if you install via `go install` or build from source. Not needed if you use a prebuilt binary.

**VMAF is optional.** If your ffmpeg was built without `libvmaf`, the VMAF step is skipped silently rather than failing. If no score appears in your report, that is why — the re-encode itself is unaffected. The score is threaded and subsampled by default, so it can differ from an exact pass by ~0.15; a filter error also falls back to the exact pass rather than failing.

## Install

### Option A — `go install` (easiest)

```sh
go install github.com/mariobgsp/vidc@latest
```

Requires Go 1.22+. The binary lands in `$(go env GOPATH)/bin`, which is often not on `PATH` by default. Add it:

```sh
export PATH="$PATH:$(go env GOPATH)/bin"
```

Add that line to `~/.bashrc` / `~/.zshrc` to persist it.

### Option B — prebuilt binary (no Go needed)

Prebuilt binaries will be published on the
[Releases page](https://github.com/mariobgsp/vidc/releases) — **as of this commit no
release has been published yet**, so this download is not available today.

Until then, the way to run vidc on a machine without Go is to build the binary here
and copy it over. vidc is a single static binary with no runtime dependencies beyond
ffmpeg, so this works across machines of the same OS/architecture:

```sh
make all          # produces dist/vidc-{linux-amd64,windows-amd64.exe,darwin-amd64,darwin-arm64}
```

Copy the one you need to the other machine, then:

```sh
chmod +x vidc-linux-amd64
sudo mv vidc-linux-amd64 /usr/local/bin/vidc
vidc -version
```

### Option C — build from source

```sh
git clone https://github.com/mariobgsp/vidc.git
cd vidc
go build -o vidc .
# or cross-compile every platform at once:
make all
```

## Quick start

Compress one file with the defaults:

```sh
vidc video.mp4 -y
```

(With no flags on a terminal you get the interactive wizard instead — see below. `-y`
skips it and uses the default `good` preset.)

Expect a report block like this (real output from a 10-second 720p clip):

```text
video.mp4: 339KB → 317KB (6% smaller), VMAF 97.74 @ 0s
  → video.min.mp4

file                                         before      after    saved     VMAF
video.mp4                                     339KB      317KB       6%    97.74
```

That is: input size → output size, percent saved, the VMAF score with its sample
offset, and the output path — followed by a summary table when you pass multiple
files. Your source file is left untouched; the compressed copy is written next to it.

## The wizard

Run `vidc` with no `-q`/`--size` on a terminal and it asks what you want. It first
probes the file, then shows the facts and a quality menu:

```text
  file      video.mp4
  1920x1080 59.94fps  h264 high  412MB
  audio     aac 2ch 128k

  Quality:
  1) fast   ~180MB   veryfast  crf20
  2) good   ~161MB   slow      crf18   [Enter]
  3) best   ~130MB   slower    crf16

  or type a target size (e.g. 8M):
```

- The facts line is what vidc detected: resolution, frame rate, video codec and
  profile, file size, then the audio line. A `rotated 90°` note appears on the facts
  line when the source carries a rotation matrix.
- Press **Enter** for `good`, type `1`/`2`/`3` or `fast`/`good`/`best`, or type a
  target size like `8M` to switch to 2-pass size mode.
- The `~` sizes are a rough heuristic — on unusual content (synthetic patterns, heavy
  grain, very static screen recordings) they can be off by more than 2x, so treat them
  as an order of magnitude, not a promise.

## Flags

| flag | default | meaning |
| --- | --- | --- |
| `-q fast\|good\|best` | `good` | quality preset |
| `--size 8M` | off | 2-pass target size; accepts `800k`, `8M`, `2G`, or bare bytes |
| `-o DIR` | beside source | write outputs into DIR instead |
| `-j N` | `min(4, NumCPU/4)` | how many files to encode in parallel |
| `--no-verify` | off | skip the VMAF measurement pass |
| `--vmaf-full` | off | exact, ~4x slower VMAF pass (default scores every 4th frame on all cores) |
| `-y` | off | skip the wizard, use defaults/flags |
| `-version` | | print the version |
| `-h` | | usage |

(`--size`, `--no-verify`, `--vmaf-full`, and `--version` also work in single-dash form: `-size`,
`-no-verify`, `-vmaf-full`, `-version`.)

Note for `-j`: x264 already uses many threads per file, so raising `-j` beyond a few
gives little and can slow each file down.

## Choosing a preset

| preset | CRF | x264 preset |
| ------ | --- | ----------- |
| fast | 20 | veryfast |
| good | 18 | slow |
| best | 16 | slower |

- `good` is the default and the right choice for most video you want to archive.
- `best` is slower and noticeably bigger; use it when the file is a master or you will
  re-edit it.
- **For video that is already well compressed (a previous export, a WhatsApp clip, a
  YouTube download), use `fast` or `--size` instead** — see the section below.

## Hitting an exact size

Some platforms enforce an upload cap. Give vidc the cap and it switches to 2-pass
target bitrate mode:

```sh
vidc talk.mp4 --size 8M      # for an 8 MB upload cap
```

The output lands near the target (measured: **1,045,550 bytes against a 1,048,576
target, −0.3%** on a 20s 720p clip). Two caveats: on very short clips 2-pass cannot
converge and may overshoot, and this mode spends whatever quality is needed to hit the
size — **VMAF can drop a long way** (measured: 55.84 on a synthetic clip, where vidc
correctly warned it was below 93). `--size` is a size tool, not a quality tool.

## Batch mode

Pass as many files as you like:

```sh
vidc ~/Videos/*.mov -q best
vidc ~/Videos/*.mp4 -j 4
vidc *.mp4 -o ~/Videos/compressed
```

Each file reports independently in its own block, then a summary table lists every
file with before/after sizes, percent saved, and VMAF. A failure in one file does not
stop the others, and the exit code is 1 if any file hard-failed.

## Output and naming

Output is `stem.min.mp4` written beside the source, auto-bumped to `.min-2.mp4`,
`.min-3.mp4`, and so on when the name is taken — it never overwrites. **The source is
never modified, moved, or deleted.**

If the output is not at least 5% smaller than the input, vidc keeps both files and
prints a loud warning naming the likely cause. That is not an error: the exit code
stays 0.

## Reading the VMAF score

VMAF is a perceptual quality metric where 100 means identical to the source. Rough
guide: **95+ very good, 93–95 acceptable, below 93 vidc warns**.

Be aware it is measured on a **30-second sample from the middle** of the file, not the
whole thing, so it is a strong signal rather than a proof. By default the score comes
from every 4th frame, threaded across all available cores; it tracks the exact score to
within ~0.15. Pass `--vmaf-full` for the exact pass, which is about 4x slower. If your ffmpeg lacks
`libvmaf`, the measurement is skipped silently and the report says so.

## Important: output can be larger

The presets target a *quality level*, not a smaller file. If your source is already
compressed at a similar or higher quality than the preset you pick, the re-encode can
produce a **larger** file. Measured example: a 1080p H.264 CRF-23 clip encoded with the
default `good` (CRF 18) grew by 33%.

vidc always detects this, keeps BOTH files, and prints a warning — it never silently
hands you a bigger file and never deletes anything. To actually shrink video that is
already well compressed, use `-q fast` (CRF 20) or `--size`.

There is no "make it smaller losslessly" mode: any real size reduction re-encodes, and
re-encoding cannot preserve the original bit-for-bit.

## Troubleshooting

| symptom | cause | fix |
| --- | --- | --- |
| `ffmpeg not found` | ffmpeg not on PATH | install it (see Requirements) |
| output is bigger than the input | source already well compressed | use `-q fast` or `--size` |
| no VMAF score shown | local ffmpeg lacks `libvmaf`, or the VMAF pass failed | nothing to fix; re-encode is unaffected |
| `VMAF ... < 93` warning | the preset or `--size` was too aggressive | use a higher preset, or drop `--size` |
| filename starting with `-` fails | the shell/ffprobe treats it as a flag | put `--` before it: `vidc -- -weird.mp4` |
| subtitles or a 2nd audio track are gone | vidc keeps only the first video and first audio track | see Limitations |
| 5.1 audio became stereo | downmixing is deliberate, for universal playback | see Limitations |

## Limitations

- Only the first video and first audio stream are kept; **subtitles, secondary audio
  tracks, and data tracks are dropped**; chapters are kept.
- 5.1 audio is downmixed to stereo on purpose, so the file plays everywhere.
- GPS coordinates, device make/model, and encoder strings are stripped. Rotation is
  applied to the pixels so orientation is preserved. Creation time is kept.
- VMAF is a 30-second sample scored every 4th frame (unless `--vmaf-full`), not the whole file.
- No lossless mode exists.
- "Visually lossless" means VMAF >= 93 typically, not bit-identity.
- Interrupting (Ctrl-C) a run may leave a partial output file and a `vidc-pass*`
  temp directory. The temp directory is reclaimed by the OS on reboot; delete the
  partial file manually, then re-run (the new output is auto-bumped to `.min-2.mp4`,
  so it will not overwrite the partial one).
- Passing the same file twice in one run is handled safely (each job reserves its own
  output name), but it re-encodes it twice — deduplicate your input list if that matters.
- The wizard's `~` size estimates are a rough heuristic. On unusual content (synthetic
  patterns, heavy grain, very static screen recordings) they can be off by more than 2x.
  Treat them as an order of magnitude, not a promise.

## Development

```sh
make all     # cross-compile all four binaries into dist/
make test    # run the test suite (needs ffmpeg; see below)
make clean   # remove dist/
```

The module is stdlib-only by design — no third-party dependencies. The tests need
ffmpeg and ffprobe on `PATH`; the end-to-end test skips itself when ffmpeg is absent.

## License

MIT — see [LICENSE](LICENSE).
