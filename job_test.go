package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/crgimenes/glaze"
)

func TestWantsGUI(t *testing.T) {
	cases := []struct {
		name               string
		forceGUI, forceCLI bool
		sawArgs, display   bool
		want               bool
	}{
		{"cli wins over gui", true, true, true, true, false},
		{"force cli with everything", false, true, true, true, false},
		{"force gui beats args", true, false, true, true, true},
		{"force gui headless", true, false, false, false, true},
		{"files mean cli", false, false, true, true, false},
		{"flags mean cli", false, false, true, false, false},
		{"no args with display", false, false, false, true, true},
		{"no args headless", false, false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wantsGUI(tc.forceGUI, tc.forceCLI, tc.sawArgs, tc.display); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIDC_CONFIG_DIR", dir)
	in := defaultSettings()
	in.Preset = "best"
	in.Size = "8M"
	in.OutDir = "/tmp/out"
	in.Verify = false
	in.WinW, in.WinH = 1024, 700
	if err := saveSettings(in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gui.json")); err != nil {
		t.Fatalf("settings file not in temp dir: %v", err)
	}
	got := loadSettings()
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %+v want %+v", got, in)
	}
}

func cancelTestClip(t *testing.T, dir, name string) string {
	t.Helper()
	src := filepath.Join(dir, name)
	gen := exec.Command("ffmpeg", "-hide_banner", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=d=10:s=640x480:r=30",
		"-f", "lavfi", "-i", "sine=f=440:d=10",
		"-c:v", "libx264", "-crf", "14", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("gen: %v\n%s", err, out)
	}
	return src
}

func cancelTestPlan(t *testing.T, src string, target int64) plan {
	t.Helper()
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
	p, _ := lookupPreset("best")
	return plan{In: src, Out: out, P: p, Target: target,
		CopyAudio: info.HasAudio && info.ACodec == "aac" && info.AChannels <= 2, Info: info}
}

func shaFile(t *testing.T, path string) [32]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(raw)
}

func TestWasCancelled(t *testing.T) {
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if wasCancelled(nil, cancelled) {
		t.Fatal("nil error with cancelled ctx must not count as cancelled: the output is complete")
	}
	if !wasCancelled(errors.New("exit status 1"), cancelled) {
		t.Fatal("failed command with cancelled ctx must count as cancelled")
	}
	if wasCancelled(errors.New("exit status 1"), context.Background()) {
		t.Fatal("failed command without cancellation must not count as cancelled")
	}
	if wasCancelled(nil, context.Background()) {
		t.Fatal("success must not count as cancelled")
	}
}

// pgrepTestChildren finds ffmpeg children launched with files under dir.
// It deliberately does NOT match the bare word "libvmaf": agent command lines
// in this environment embed brief text containing it, which pgrep -f would match.
// The t.TempDir() path is unique to this test and appears in the child's -i args.
func pgrepTestChildren(t *testing.T, dir string) []string {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", dir).Output()
	if err != nil {
		return nil
	}
	var pids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == itoa(os.Getpid()) {
			continue
		}
		comm, err := exec.Command("ps", "-o", "comm=", "-p", line).Output()
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(comm)) == "ffmpeg" {
			pids = append(pids, line)
		}
	}
	return pids
}

func pidGone(pid string) bool {
	return exec.Command("ps", "-p", pid).Run() != nil
}

// TestVMAFCancelKillsChild proves a Cancel during the VMAF phase kills the
// measuring ffmpeg child instead of detaching it to run to completion.
func TestVMAFCancelKillsChild(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg absent")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe absent")
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep absent")
	}
	if !vmafAvailable() {
		t.Skip("libvmaf absent")
	}
	dir := t.TempDir()
	src := cancelTestClip(t, dir, "v.mp4")
	info, err := probe(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	type vr struct {
		score float64
		off   time.Duration
		ok    bool
	}
	ch := make(chan vr, 1)
	go func() {
		s, off, ok := measureVMAF(ctx, src, src, info.Duration)
		ch <- vr{s, off, ok}
	}()
	var pids []string
	deadline := time.Now().Add(15 * time.Second)
	for len(pids) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		pids = pgrepTestChildren(t, dir)
	}
	if len(pids) == 0 {
		cancel()
		t.Fatal("no libvmaf child appeared; cannot prove the kill")
	}
	cancel()
	start := time.Now()
	r := <-ch
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("measureVMAF took %v after cancel; child was detached, not killed", took)
	}
	if r.ok {
		t.Fatal("cancelled VMAF must return ok=false")
	}
	goneBy := time.Now().Add(5 * time.Second)
	for {
		allGone := true
		for _, pid := range pids {
			if !pidGone(pid) {
				allGone = false
				break
			}
		}
		if allGone {
			return
		}
		if time.Now().After(goneBy) {
			t.Fatalf("libvmaf children survived cancel: %v", pids)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func passDirs(t *testing.T) map[string]bool {
	t.Helper()
	es, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]bool{}
	for _, e := range es {
		if strings.HasPrefix(e.Name(), "vidc-pass") {
			m[e.Name()] = true
		}
	}
	return m
}

func TestRunJobCancel(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg absent")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe absent")
	}
	dir := t.TempDir()
	src := cancelTestClip(t, dir, "src.mp4")
	before := shaFile(t, src)

	t.Run("crf", func(t *testing.T) {
		pl := cancelTestPlan(t, src, 0)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(700 * time.Millisecond)
			cancel()
		}()
		err := runJob(ctx, pl, nil)
		if !errors.Is(err, errCancelled) {
			t.Fatalf("got %v want errCancelled", err)
		}
		if _, statErr := os.Stat(pl.Out); !os.IsNotExist(statErr) {
			t.Fatalf("partial output kept: %s", pl.Out)
		}
		if got := shaFile(t, src); got != before {
			t.Fatal("source file was touched")
		}
	})

	t.Run("size", func(t *testing.T) {
		pl := cancelTestPlan(t, src, 8<<20)
		pre := passDirs(t)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(700 * time.Millisecond)
			cancel()
		}()
		err := runJob(ctx, pl, nil)
		if !errors.Is(err, errCancelled) {
			t.Fatalf("got %v want errCancelled", err)
		}
		if _, statErr := os.Stat(pl.Out); !os.IsNotExist(statErr) {
			t.Fatalf("partial output kept: %s", pl.Out)
		}
		if got := shaFile(t, src); got != before {
			t.Fatal("source file was touched")
		}
		if post := passDirs(t); !reflect.DeepEqual(post, pre) {
			t.Fatalf("temp dirs leaked: before %v after %v", pre, post)
		}
	})
}

// buildTestBinary compiles the current package once into a temp dir.
func buildTestBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "vidc-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test binary: %v\n%s", err, out)
	}
	return bin
}

// TestCLIExitsOnEOF guards the bug where `vidc </dev/null` looped forever printing
// "video path: ". It runs the built binary with stdin at EOF and fails if it does not
// exit promptly.
func TestCLIExitsOnEOF(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := buildTestBinary(t)
	cmd := exec.Command(bin)
	cmd.Stdin = strings.NewReader("") // immediate EOF
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Env = append(os.Environ(), "VIDC_CONFIG_DIR="+t.TempDir(),
		"DISPLAY=", "WAYLAND_DISPLAY=") // force the CLI path, not the GUI
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// exited: good. assert it did not spam
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("binary did not exit within 20s with stdin at EOF (the runaway-wizard bug); output so far: %d bytes", out.Len())
	}
	if out.Len() > 64*1024 {
		t.Fatalf("wrote %d bytes to stdout on EOF; expected a short usage message", out.Len())
	}
}

// stubWV is a minimal glaze.WebView for pinning the bound JS names headlessly.
type stubWV struct {
	bound []string
}

func (s *stubWV) Run()                          {}
func (s *stubWV) Terminate()                    {}
func (s *stubWV) Dispatch(f func())             { f() }
func (s *stubWV) Destroy()                      {}
func (s *stubWV) Window() unsafe.Pointer        { return nil }
func (s *stubWV) SetTitle(string)               {}
func (s *stubWV) SetSize(int, int, glaze.Hint)  {}
func (s *stubWV) Navigate(string)               {}
func (s *stubWV) SetHtml(string)                {}
func (s *stubWV) Init(string)                   {}
func (s *stubWV) Eval(string)                   {}
func (s *stubWV) Focus()                        {}
func (s *stubWV) Raise()                        {}
func (s *stubWV) Bind(name string, _ any) error { s.bound = append(s.bound, name); return nil }
func (s *stubWV) Unbind(string) error           { return nil }
func (s *stubWV) OpenFile(glaze.FileDialogOptions) (string, error) {
	return "", nil
}
func (s *stubWV) OpenFiles(glaze.FileDialogOptions) ([]string, error) {
	return nil, nil
}
func (s *stubWV) SaveFile(glaze.FileDialogOptions) (string, error) {
	return "", nil
}
func (s *stubWV) OpenDirectory(glaze.FileDialogOptions) (string, error) {
	return "", nil
}

func TestBindNames(t *testing.T) {
	stub := &stubWV{}
	api := newGUIAPI(nil, defaultSettings())
	names, err := glaze.BindMethods(stub, "vidc", api)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vidc_add_files",
		"vidc_cancel",
		"vidc_choose_out_dir",
		"vidc_get_state",
		"vidc_note_window_size",
		"vidc_open_folder",
		"vidc_remove_file",
		"vidc_start",
		"vidc_update_settings",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("got %q want %q", names, want)
	}
}
