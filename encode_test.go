package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testInfo() *Info {
	return &Info{
		Duration:     60 * time.Second,
		Width:        1920,
		Height:       1080,
		FPS:          30,
		HasVideo:     true,
		HasAudio:     true,
		ACodec:       "aac",
		AChannels:    2,
		ABitrate:     128000,
		CreationTime: "2024-01-01T00:00:00.000Z",
	}
}

func TestBuildArgs(t *testing.T) {
	mkplan := func(name string, copyAudio, hasAudio bool, ct string) plan {
		var p preset
		for _, pr := range presets {
			if pr.Name == name {
				p = pr
			}
		}
		info := testInfo()
		info.HasAudio = hasAudio
		info.CreationTime = ct
		return plan{In: "in.mp4", Out: "in.min.mp4", P: p, CopyAudio: copyAudio, Info: info}
	}
	cases := []struct {
		name string
		pl   plan
		pass int
		log  string
		want []string
	}{
		{
			"good/copy/ct",
			mkplan("good", true, true, "2024-01-01T00:00:00.000Z"),
			0, "",
			[]string{"ffmpeg", "-hide_banner", "-y", "-i", "in.mp4",
				"-map", "0:v:0", "-map", "0:a:0?",
				"-map_metadata", "-1", "-metadata", "creation_time=2024-01-01T00:00:00.000Z",
				"-map_chapters", "0",
				"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p",
				"-crf", "18", "-preset", "slow",
				"-c:a", "copy",
				"-movflags", "+faststart",
				"-progress", "pipe:1", "-nostats", "-loglevel", "error", "--", "in.min.mp4"},
		},
		{
			"fast/transcode/no-ct",
			mkplan("fast", false, true, ""),
			0, "",
			[]string{"ffmpeg", "-hide_banner", "-y", "-i", "in.mp4",
				"-map", "0:v:0", "-map", "0:a:0?",
				"-map_metadata", "-1",
				"-map_chapters", "0",
				"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p",
				"-crf", "20", "-preset", "veryfast",
				"-c:a", "aac", "-b:a", "128k", "-ac", "2",
				"-movflags", "+faststart",
				"-progress", "pipe:1", "-nostats", "-loglevel", "error", "--", "in.min.mp4"},
		},
		{
			"best/transcode/ct",
			mkplan("best", false, true, "2024-01-01T00:00:00.000Z"),
			0, "",
			[]string{"ffmpeg", "-hide_banner", "-y", "-i", "in.mp4",
				"-map", "0:v:0", "-map", "0:a:0?",
				"-map_metadata", "-1", "-metadata", "creation_time=2024-01-01T00:00:00.000Z",
				"-map_chapters", "0",
				"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p",
				"-crf", "16", "-preset", "slower",
				"-c:a", "aac", "-b:a", "128k", "-ac", "2",
				"-movflags", "+faststart",
				"-progress", "pipe:1", "-nostats", "-loglevel", "error", "--", "in.min.mp4"},
		},
		{
			"good/no-audio",
			mkplan("good", false, false, ""),
			0, "",
			[]string{"ffmpeg", "-hide_banner", "-y", "-i", "in.mp4",
				"-map", "0:v:0", "-map", "0:a:0?",
				"-map_metadata", "-1",
				"-map_chapters", "0",
				"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p",
				"-crf", "18", "-preset", "slow",
				"-movflags", "+faststart",
				"-progress", "pipe:1", "-nostats", "-loglevel", "error", "--", "in.min.mp4"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildArgs(tc.pl, tc.pass, tc.log); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}

	// size mode: pass1 ends with -an -f null -, pass2 ends with OUT
	sz := mkplan("good", false, true, "")
	sz.Target = 8 << 20
	p1 := buildArgs(sz, 1, "/tmp/x/pass")
	if got := p1[len(p1)-6:]; !reflect.DeepEqual(got, []string{"-pass", "1", "-passlogfile", "/tmp/x/pass", "-an", "-f"}) && p1[len(p1)-1] != "-" {
		t.Fatalf("pass1 tail wrong: %q", p1)
	}
	if p1[len(p1)-1] != "-" || p1[len(p1)-2] != "null" {
		t.Fatalf("pass1 must end -f null -: %q", p1)
	}
	if !strings.Contains(strings.Join(p1, " "), "-b:v ") {
		t.Fatalf("pass1 missing -b:v: %q", p1)
	}
	if strings.Contains(strings.Join(p1, " "), "pipe:1") {
		t.Fatalf("pass1 must not have -progress: %q", p1)
	}
	p2 := buildArgs(sz, 2, "/tmp/x/pass")
	if p2[len(p2)-1] != "in.min.mp4" {
		t.Fatalf("pass2 must end with OUT: %q", p2)
	}
	if !strings.Contains(strings.Join(p2, " "), "-pass 2") {
		t.Fatalf("pass2 missing -pass 2: %q", p2)
	}
	if !strings.Contains(strings.Join(p2, " "), "pipe:1") {
		t.Fatalf("pass2 missing -progress: %q", p2)
	}
	if !strings.Contains(strings.Join(p2, " "), "+faststart") {
		t.Fatalf("pass2 missing faststart: %q", p2)
	}

	// creation_time present vs absent
	with := buildArgs(mkplan("good", true, true, "X"), 0, "")
	without := buildArgs(mkplan("good", true, true, ""), 0, "")
	if !strings.Contains(strings.Join(with, " "), "creation_time=X") {
		t.Fatalf("expected creation_time in %q", with)
	}
	if strings.Contains(strings.Join(without, " "), "creation_time") {
		t.Fatalf("unexpected creation_time in %q", without)
	}

	// all presets copy + transcode covered: fast/best copy variants
	for _, pr := range presets {
		pl := mkplan(pr.Name, true, true, "")
		got := buildArgs(pl, 0, "")
		if !strings.Contains(strings.Join(got, " "), "-crf "+itoa(pr.CRF)) {
			t.Fatalf("%s: missing crf in %q", pr.Name, got)
		}
		pl2 := mkplan(pr.Name, false, true, "")
		got2 := buildArgs(pl2, 0, "")
		if !strings.Contains(strings.Join(got2, " "), "-c:a aac") {
			t.Fatalf("%s: missing aac transcode in %q", pr.Name, got2)
		}
	}
}

func TestVMAFFilter(t *testing.T) {
	want := "[0:v]setpts=PTS-STARTPTS[d];[1:v]setpts=PTS-STARTPTS[r];[d][r]libvmaf=" + fmt.Sprintf("n_threads=%d:n_subsample=4", runtime.GOMAXPROCS(0))
	if fast := vmafFilter(false); fast != want {
		t.Fatalf("fast filter changed: got %s, want %s", fast, want)
	}
	full := vmafFilter(true)
	if full != "[0:v]setpts=PTS-STARTPTS[d];[1:v]setpts=PTS-STARTPTS[r];[d][r]libvmaf" {
		t.Fatalf("full filter changed: %s", full)
	}
	for _, bad := range []string{"n_threads", "n_subsample"} {
		if strings.Contains(full, bad) {
			t.Fatalf("full filter must not contain %q: %s", bad, full)
		}
	}
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func TestNextOutput(t *testing.T) {
	dir := t.TempDir()
	touch := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	touch("a.min.mp4")
	got, err := nextOutput(filepath.Join(dir, "a.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "a.min-2.mp4" {
		t.Fatalf("got %s", got)
	}
	touch("a.min-2.mp4")
	got, err = nextOutput(filepath.Join(dir, "a.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "a.min-3.mp4" {
		t.Fatalf("got %s", got)
	}
	got, err = nextOutput(filepath.Join(dir, "video.min.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "video.min.min.mp4" {
		t.Fatalf("got %s", got)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"8M": 8 << 20, "8MB": 8 << 20, "800k": 800 << 10,
		"2G": 2 << 30, "1024": 1024,
	} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Fatalf("%s: got %d,%v want %d", in, got, err, want)
		}
	}
	if _, err := parseSize("nonsense"); err == nil {
		t.Fatal("expected error for nonsense")
	}
}

func TestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg absent")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe absent")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	gen := exec.Command("ffmpeg", "-hide_banner", "-y",
		"-f", "lavfi", "-i", "testsrc=d=2:s=320x240:r=30",
		"-f", "lavfi", "-i", "sine=f=440:d=2",
		"-c:v", "libx264", "-crf", "14", "-c:a", "aac", "-shortest", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("gen: %v\n%s", err, out)
	}
	info, err := probe(src)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(src); err == nil {
		info.Size = fi.Size()
	}
	out, err := nextOutput(src)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := lookupPreset("good")
	pl := plan{In: src, Out: out, P: p, CopyAudio: info.HasAudio && info.ACodec == "aac" && info.AChannels <= 2, Info: info}
	if err := runEncode(pl, testWriter{t}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	of, err := os.Stat(out)
	if err != nil {
		t.Fatalf("no output: %v", err)
	}
	sf, _ := os.Stat(src)
	if of.Size() >= sf.Size() {
		t.Fatalf("output %d not smaller than source %d", of.Size(), sf.Size())
	}
	back, err := probe(out)
	if err != nil {
		t.Fatal(err)
	}
	if back.VCodec != "h264" || back.PixFmt != "yuv420p" || back.ACodec != "aac" {
		t.Fatalf("bad streams: %+v", back)
	}
	if !(back.HasVideo && back.HasAudio) {
		t.Fatalf("missing streams: %+v", back)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"flags first unchanged", []string{"-y", "-q", "fast", "a.mp4"}, []string{"-y", "-q", "fast", "a.mp4"}},
		{"flag after file", []string{"a.mp4", "-y"}, []string{"-y", "a.mp4"}},
		{"valued flag after file", []string{"a.mp4", "-q", "fast"}, []string{"-q", "fast", "a.mp4"}},
		{"two files reorder", []string{"a.mp4", "b.mp4", "-q", "fast", "-o", "out"}, []string{"-q", "fast", "-o", "out", "a.mp4", "b.mp4"}},
		{"long valued flag", []string{"a.mp4", "--size", "8M"}, []string{"--size", "8M", "a.mp4"}},
		{"equals form kept", []string{"a.mp4", "--size=8M"}, []string{"--size=8M", "a.mp4"}},
		{"dashdash terminator", []string{"-y", "-q", "fast", "--", "-dash.mp4"}, []string{"-y", "-q", "fast", "--", "-dash.mp4"}},
		{"lone dash is file", []string{"-", "a.mp4"}, []string{"-", "a.mp4"}},
		{"empty", []string{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitArgs(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func buildTestBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "vidc-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test binary: %v\n%s", err, out)
	}
	return bin
}

// TestCLIExitsOnEOF guards a crash-class bug: with stdin at EOF the wizard
// printed "video path: " forever (264 MB in 25 s, spinning a CPU core) because
// ReadString's error was discarded and EOF looked like a blank line. It also
// asserts on BYTES WRITTEN, so a fast runaway loop cannot slip past the timeout
// unnoticed. This is the standard non-interactive shape: cron, a script,
// `ssh host vidc`, or `vidc </dev/null`.
func TestCLIExitsOnEOF(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := buildTestBinary(t)
	cmd := exec.Command(bin)
	cmd.Stdin = strings.NewReader("") // immediate EOF
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// exited: good, now assert it did not spam
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("binary did not exit within 20s with stdin at EOF (runaway-wizard bug); output so far: %d bytes", out.Len())
	}
	if out.Len() > 64*1024 {
		t.Fatalf("wrote %d bytes to stdout on EOF; expected a short usage message", out.Len())
	}
}
