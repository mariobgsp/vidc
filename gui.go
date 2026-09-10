package main

// gui.go — the vidc GUI window (Glaze WebView) plus the per-file encode flow.
//
// The GUI reuses the same runJob as the CLI, so there is one encode code path.
// Orchestration here (probe → reserve → runJob → VMAF) mirrors processOne; the
// exec+parse+kill logic itself lives only in job.go.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/crgimenes/glaze"
)

//go:embed ui.html
var uiHTML string

// guiSettings persists across runs in os.UserConfigDir()/vidc/gui.json.
// VIDC_CONFIG_DIR overrides the directory (used by tests).
type guiSettings struct {
	Preset string `json:"preset"`
	Size   string `json:"size"`
	OutDir string `json:"outdir"`
	Verify bool   `json:"verify"`
	WinW   int    `json:"win_w"`
	WinH   int    `json:"win_h"`
}

func defaultSettings() guiSettings {
	return guiSettings{Preset: "good", Verify: true, WinW: 900, WinH: 640}
}

func configDir() string {
	if d := os.Getenv("VIDC_CONFIG_DIR"); d != "" {
		return d
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "."
	}
	return filepath.Join(d, "vidc")
}

func settingsPath() string {
	return filepath.Join(configDir(), "gui.json")
}

func loadSettings() guiSettings {
	st := defaultSettings()
	raw, err := os.ReadFile(settingsPath())
	if err != nil {
		return st
	}
	var loaded guiSettings
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return st
	}
	if loaded.Preset == "" {
		// Empty config (e.g. a literal `null` unmarshals to the zero struct):
		// keep the built-in defaults instead of adopting zero values.
		return st
	}
	if _, ok := lookupPreset(loaded.Preset); ok {
		st.Preset = loaded.Preset
	}
	st.Size = loaded.Size
	st.OutDir = loaded.OutDir
	st.Verify = loaded.Verify
	if loaded.WinW >= 400 && loaded.WinH >= 300 {
		st.WinW, st.WinH = loaded.WinW, loaded.WinH
	}
	return st
}

func saveSettings(st guiSettings) error {
	if err := os.MkdirAll(configDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath(), raw, 0o600)
}

// guiFile is one row in the window. Path is never sent to JS.
type guiFile struct {
	ID      string  `json:"id"`
	Path    string  `json:"-"`
	Name    string  `json:"name"`
	Status  string  `json:"status"` // queued|running|done|error|cancelled
	Percent float64 `json:"percent"`
	Speed   float64 `json:"speed"`
	ETA     string  `json:"eta"`
	Result  string  `json:"result,omitempty"`
	VMAF    string  `json:"vmaf,omitempty"`
	Warn    string  `json:"warn,omitempty"`
	Err     string  `json:"error,omitempty"`
	Out     string  `json:"-"`
}

type guiStateView struct {
	Files    []guiFile   `json:"files"`
	Settings guiSettings `json:"settings"`
	Running  bool        `json:"running"`
	Summary  string      `json:"summary"`
}

type startOpts struct {
	Preset string `json:"preset"`
	Size   string `json:"size"`
	OutDir string `json:"outdir"`
	Verify bool   `json:"verify"`
}

type progressEvent struct {
	ID      string  `json:"id"`
	Percent float64 `json:"percent"`
	Speed   float64 `json:"speed"`
	ETA     string  `json:"eta"`
}

type resultEvent struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result string `json:"result,omitempty"`
	VMAF   string `json:"vmaf,omitempty"`
	Warn   string `json:"warn,omitempty"`
	Error  string `json:"error,omitempty"`
}

type doneEvent struct {
	Summary   string `json:"summary"`
	Failed    int    `json:"failed"`
	Cancelled bool   `json:"cancelled"`
}

var videoFilter = []glaze.FileFilter{
	{Name: "Videos", Extensions: []string{"mp4", "mov", "mkv", "avi", "webm", "m4v", "flv", "wmv", "mpg", "mpeg", "ts", "3gp"}},
}

type guiAPI struct {
	mu       sync.Mutex
	w        glaze.WebView
	ev       *glaze.Events
	files    []*guiFile
	settings guiSettings
	summary  string
	running  bool
	cancel   context.CancelFunc
}

func newGUIAPI(w glaze.WebView, st guiSettings) *guiAPI {
	return &guiAPI{w: w, settings: st}
}

func (a *guiAPI) snapshotLocked() []guiFile {
	out := make([]guiFile, len(a.files))
	for i, f := range a.files {
		out[i] = *f
	}
	return out
}

// AddFiles opens the native file picker and adds the chosen videos.
// Files are deduplicated by absolute path. Cancelling returns the current list.
func (a *guiAPI) AddFiles() ([]guiFile, error) {
	paths, err := a.w.OpenFiles(glaze.FileDialogOptions{Title: "Add videos", Filters: videoFilter})
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addPathsLocked(paths)
	return a.snapshotLocked(), nil
}

// ChooseOutDir opens the native directory picker for the output directory.
func (a *guiAPI) ChooseOutDir() (string, error) {
	dir, err := a.w.OpenDirectory(glaze.FileDialogOptions{Title: "Output directory"})
	if err != nil {
		return "", err
	}
	if dir == "" {
		return "", nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settings.OutDir = dir
	_ = saveSettings(a.settings)
	return dir, nil
}

// RemoveFile drops an idle row. Running rows cannot be removed.
func (a *guiAPI) RemoveFile(id string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, f := range a.files {
		if f.ID == id {
			if f.Status == "running" {
				return false, nil
			}
			a.files = append(a.files[:i], a.files[i+1:]...)
			a.emitLocked("vidc:files", a.snapshotLocked())
			return true, nil
		}
	}
	return false, nil
}

// GetState returns the full UI snapshot for initial render and refresh.
func (a *guiAPI) GetState() (guiStateView, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return guiStateView{Files: a.snapshotLocked(), Settings: a.settings, Running: a.running, Summary: a.summary}, nil
}

// UpdateSettings stores settings changes from the window and persists them.
func (a *guiAPI) UpdateSettings(s guiSettings) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := lookupPreset(s.Preset); ok {
		a.settings.Preset = s.Preset
	}
	a.settings.Size = s.Size
	a.settings.OutDir = s.OutDir
	a.settings.Verify = s.Verify
	if err := saveSettings(a.settings); err != nil {
		return "", err
	}
	return "saved", nil
}

// NoteWindowSize records a resize so the next launch restores it.
func (a *guiAPI) NoteWindowSize(w, h int) (string, error) {
	if w < 400 || h < 300 {
		return "", errors.New("implausible window size")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settings.WinW, a.settings.WinH = w, h
	if err := saveSettings(a.settings); err != nil {
		return "", err
	}
	return "saved", nil
}

// Start validates settings, persists them, and encodes in the background.
func (a *guiAPI) Start(o startOpts) (string, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return "", errors.New("already running")
	}
	if len(a.files) == 0 {
		a.mu.Unlock()
		return "", errors.New("no files")
	}
	p, target, outDir, verify, err := resolveOpts(o.Preset, o.Size, o.OutDir, o.Verify)
	if err != nil {
		a.mu.Unlock()
		return "", err
	}
	for _, f := range a.files {
		if f.Status == "running" {
			a.mu.Unlock()
			return "", errors.New("already running")
		}
		f.Status = "queued"
		f.Percent, f.Speed = 0, 0
		f.ETA, f.Result, f.VMAF, f.Warn, f.Err = "", "", "", "", ""
	}
	a.settings.Preset, a.settings.Size, a.settings.OutDir, a.settings.Verify = p.Name, o.Size, outDir, verify
	_ = saveSettings(a.settings)
	a.summary = ""
	a.running = true
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.mu.Unlock()
	go a.runAll(ctx, p, target, outDir, verify)
	return "started", nil
}

// Cancel stops all in-flight jobs. Safe to press twice.
func (a *guiAPI) Cancel() (string, error) {
	a.mu.Lock()
	c := a.cancel
	running := a.running
	a.mu.Unlock()
	if !running || c == nil {
		return "idle", nil
	}
	c()
	return "cancelled", nil
}

// OpenFolder opens the output directory in the OS file manager.
func (a *guiAPI) OpenFolder() (string, error) {
	a.mu.Lock()
	dir := a.settings.OutDir
	for _, f := range a.files {
		if f.Out != "" {
			dir = filepath.Dir(f.Out)
			break
		}
	}
	a.mu.Unlock()
	if dir == "" {
		dir = "."
	}
	if err := openPath(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// resolveOpts applies the same preset/size precedence as the CLI.
func resolveOpts(presetName, sizeStr, outDir string, verify bool) (preset, int64, string, bool, error) {
	p, _ := lookupPreset("good")
	if presetName != "" {
		var ok bool
		if p, ok = lookupPreset(presetName); !ok {
			return preset{}, 0, "", false, errors.New("unknown preset")
		}
	}
	var target int64
	if sizeStr != "" {
		p, _ = lookupPreset("good")
		if presetName != "" {
			var ok bool
			if p, ok = lookupPreset(presetName); !ok {
				return preset{}, 0, "", false, errors.New("unknown preset")
			}
		}
		var err error
		if target, err = parseSize(sizeStr); err != nil {
			return preset{}, 0, "", false, err
		}
	}
	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return preset{}, 0, "", false, err
		}
	}
	return p, target, outDir, verify, nil
}

func (a *guiAPI) addPathsLocked(paths []string) {
	seen := map[string]bool{}
	for _, f := range a.files {
		seen[f.ID] = true
	}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		a.files = append(a.files, &guiFile{ID: abs, Path: p, Name: filepath.Base(p), Status: "queued"})
	}
}

func (a *guiAPI) emitLocked(name string, data any) {
	ev := a.ev
	if ev == nil {
		return
	}
	// Emit is goroutine-safe; never hold a.mu across it.
	a.mu.Unlock()
	_ = ev.Emit(name, data)
	a.mu.Lock()
}

func (a *guiAPI) setStatus(gf *guiFile, status string) {
	a.mu.Lock()
	gf.Status = status
	snap := a.snapshotLocked()
	a.emitLocked("vidc:files", snap)
	a.mu.Unlock()
}

func (a *guiAPI) runAll(ctx context.Context, p preset, target int64, outDir string, verify bool) {
	defer func() {
		a.mu.Lock()
		a.running = false
		a.cancel = nil
		a.mu.Unlock()
	}()
	a.mu.Lock()
	queue := append([]*guiFile(nil), a.files...)
	a.mu.Unlock()
	sem := make(chan struct{}, min(4, max(1, runtime.NumCPU()/4)))
	var wg sync.WaitGroup
	for _, gf := range queue {
		wg.Add(1)
		go func(gf *guiFile) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				a.setStatus(gf, "cancelled")
				return
			}
			a.runOne(ctx, p, target, outDir, verify, gf)
		}(gf)
	}
	wg.Wait()
	a.finish(ctx)
}

func (a *guiAPI) failFile(gf *guiFile, msg string) {
	a.mu.Lock()
	gf.Status = "error"
	gf.Err = msg
	res := resultEvent{ID: gf.ID, Status: "error", Error: msg}
	a.emitLocked("vidc:result", res)
	snap := a.snapshotLocked()
	a.emitLocked("vidc:files", snap)
	a.mu.Unlock()
}

// runOne encodes a single file. Orchestration mirrors processOne; the
// exec+parse+kill core is runJob in job.go.
func (a *guiAPI) runOne(ctx context.Context, p preset, target int64, outDir string, verify bool, gf *guiFile) {
	a.setStatus(gf, "running")
	info, err := probe(gf.Path)
	if err != nil {
		a.failFile(gf, err.Error())
		return
	}
	fi, err := os.Stat(gf.Path)
	if err != nil {
		a.failFile(gf, err.Error())
		return
	}
	info.Size = fi.Size()
	if info.Size <= 0 {
		a.failFile(gf, gf.Path+": empty or unreadable file")
		return
	}
	var out string
	if outDir != "" {
		ext := filepath.Ext(gf.Path)
		stem := strings.TrimSuffix(filepath.Base(gf.Path), ext)
		if out, err = reserveName(outDir, stem, gf.Path); err != nil {
			a.failFile(gf, err.Error())
			return
		}
	} else {
		if out, err = nextOutput(gf.Path); err != nil {
			a.failFile(gf, err.Error())
			return
		}
	}
	success := false
	defer func() {
		if !success {
			os.Remove(out)
		}
	}()
	copyAudio := info.HasAudio && strings.ToLower(info.ACodec) == "aac" && info.AChannels <= 2 && info.AChannels > 0
	pl := plan{In: gf.Path, Out: out, P: p, Target: target, CopyAudio: copyAudio, Info: info}
	onProgress := func(pr Progress) {
		eta := ""
		if pr.Speed > 0 && !pr.Done {
			eta = pr.ETA.Round(time.Second).String()
		}
		a.mu.Lock()
		gf.Percent, gf.Speed, gf.ETA = pr.Percent, pr.Speed, eta
		pe := progressEvent{ID: gf.ID, Percent: pr.Percent, Speed: pr.Speed, ETA: eta}
		a.emitLocked("vidc:progress", pe)
		a.mu.Unlock()
	}
	if err := runJob(ctx, pl, onProgress); err != nil {
		if errors.Is(err, errCancelled) || ctx.Err() != nil {
			a.setStatus(gf, "cancelled")
			return
		}
		a.failFile(gf, err.Error())
		return
	}
	of, err := os.Stat(out)
	if err != nil {
		a.failFile(gf, err.Error())
		return
	}
	before, after := info.Size, of.Size()
	saved := 100 * (1 - float64(after)/float64(before))
	vmafText, warn := "skipped", ""
	if verify {
		if !vmafAvailable() {
			vmafText = "unavailable"
		} else {
			score, _, ok := vmafWithCancel(ctx, gf.Path, out, info.Duration)
			if ctx.Err() != nil {
				// Encode produced a complete output: keep it, stop measuring.
				success = true
				a.mu.Lock()
				gf.Out = out
				a.mu.Unlock()
				a.setStatus(gf, "cancelled")
				return
			}
			if ok {
				vmafText = fmt.Sprintf("%.1f", score)
				if score < 93 {
					warn = "VMAF " + vmafText + " < 93 — quality may be visibly degraded"
				}
			} else {
				vmafText = "error"
			}
		}
	}
	delta := "-" + numStr(int(saved+0.5)) + "%"
	if saved < 0 {
		delta = "+" + numStr(int(-saved+0.5)) + "%"
	}
	text := humanSize(before) + " → " + humanSize(after) + " (" + delta + ")"
	if after >= int64(float64(before)*0.95) {
		warn = appendWarn(warn, "Output is not at least 5% smaller ("+humanSize(before)+" → "+humanSize(after)+"). Likely cause: the source is already well compressed. Both files were kept; nothing was deleted.")
	}
	success = true
	a.mu.Lock()
	gf.Out = out
	gf.Status = "done"
	gf.Percent = 100
	gf.Result = text
	gf.VMAF = vmafText
	gf.Warn = warn
	res := resultEvent{ID: gf.ID, Status: "done", Result: text, VMAF: vmafText, Warn: warn}
	a.emitLocked("vidc:result", res)
	snap := a.snapshotLocked()
	a.emitLocked("vidc:files", snap)
	a.mu.Unlock()
}

func appendWarn(warn, extra string) string {
	if warn == "" {
		return extra
	}
	return warn + "\n" + extra
}

func numStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// vmafWithCancel waits for a VMAF measurement but stops waiting on ctx.
// The output file is complete by then, so cancelling keeps it.
func vmafWithCancel(ctx context.Context, orig, enc string, d time.Duration) (float64, time.Duration, bool) {
	if ctx.Err() != nil {
		return 0, 0, false
	}
	type res struct {
		score float64
		off   time.Duration
		ok    bool
	}
	ch := make(chan res, 1)
	go func() {
		s, off, ok := measureVMAF(ctx, orig, enc, d)
		ch <- res{s, off, ok}
	}()
	select {
	case r := <-ch:
		return r.score, r.off, r.ok
	case <-ctx.Done():
		return 0, 0, false
	}
}

func (a *guiAPI) finish(ctx context.Context) {
	a.mu.Lock()
	done, failed := 0, 0
	for _, f := range a.files {
		switch f.Status {
		case "done":
			done++
		case "error":
			failed++
		}
	}
	// Recompute totals from result text is fragile; track sizes instead.
	var tb, ta int64
	for _, f := range a.files {
		if f.Status != "done" {
			continue
		}
		if fi, err := os.Stat(f.Path); err == nil {
			tb += fi.Size()
		}
		if fo, err := os.Stat(f.Out); err == nil {
			ta += fo.Size()
		}
	}
	summary := ""
	cancelled := ctx.Err() != nil
	switch {
	case cancelled && done == 0 && failed == 0:
		summary = "cancelled, nothing kept"
	case tb > 0:
		saved := 100 * (1 - float64(ta)/float64(tb))
		delta := "-" + numStr(int(saved+0.5)) + "%"
		if saved < 0 {
			delta = "+" + numStr(int(-saved+0.5)) + "%"
		}
		summary = "Done: " + numStr(done) + " file" + plural(done) + ", " + humanSize(tb) + " → " + humanSize(ta) + " (" + delta + ")"
		if failed > 0 {
			summary += ", " + numStr(failed) + " failed"
		}
		if cancelled {
			summary = "cancelled — kept " + numStr(done) + " file" + plural(done) + ": " + summary
		}
	default:
		if failed > 0 {
			summary = numStr(failed) + " file" + plural(failed) + " failed"
		}
	}
	a.summary = summary
	d := doneEvent{Summary: summary, Failed: failed, Cancelled: cancelled}
	a.emitLocked("vidc:done", d)
	a.mu.Unlock()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// openPath opens dir in the OS file manager. No new dependency.
func openPath(dir string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", dir)
	case "windows":
		cmd = exec.Command("explorer", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	return cmd.Start()
}

func printWebviewHint() {
	fmt.Fprintln(os.Stderr, "vidc: could not open a GUI window (WebView runtime missing?).")
	switch runtime.GOOS {
	case "linux":
		fmt.Fprintln(os.Stderr, "Install the WebView system library for your distro, then retry:")
		fmt.Fprintln(os.Stderr, "  Arch/Omarchy:    pacman -S webkit2gtk-4.1")
		fmt.Fprintln(os.Stderr, "  Debian/Ubuntu:   sudo apt install libwebkit2gtk-4.1-0")
		fmt.Fprintln(os.Stderr, "  Fedora:          sudo dnf install webkit2gtk4.1")
	case "darwin":
		fmt.Fprintln(os.Stderr, "Install the Xcode command line tools (WebKit) and retry.")
	case "windows":
		fmt.Fprintln(os.Stderr, "Install the WebView2 runtime and retry.")
	default:
		fmt.Fprintln(os.Stderr, "Install your platform's WebView runtime and retry.")
	}
}

// runGUI opens the vidc window (900x640, title "vidc"), serves the embedded
// UI, and runs the main loop. Returns a process exit code, never panics.
func runGUI(files []string) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "vidc: GUI failed: %v\n", r)
			code = 1
		}
	}()
	st := loadSettings()
	w, err := glaze.New(false)
	if err != nil {
		printWebviewHint()
		return 1
	}
	defer w.Destroy()
	api := newGUIAPI(w, st)
	api.addPathsLocked(files)
	ev, err := glaze.NewEvents(w)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vidc: GUI events failed: %v\n", err)
		return 1
	}
	api.ev = ev
	bound, err := glaze.BindMethods(w, "vidc", api)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vidc: GUI bindings failed: %v\n", err)
		return 1
	}
	fmt.Println("vidc GUI bindings:", strings.Join(bound, " "))
	w.SetTitle("vidc")
	w.SetSize(st.WinW, st.WinH, glaze.HintNone)
	w.SetHtml(uiHTML)
	w.Run()
	return 0
}
