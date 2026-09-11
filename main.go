package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

var version = "dev"

// ponytail: bpp heuristic, calibrate against real output if estimates look off
var estBPP = map[int]float64{20: 0.045, 18: 0.065, 16: 0.115}

var outMu sync.Mutex

func emit(w io.Writer, format string, a ...any) {
	outMu.Lock()
	defer outMu.Unlock()
	fmt.Fprintf(w, format, a...)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("vidc", flag.ContinueOnError)
	q := fs.String("q", "", "quality preset: fast|good|best")
	size := fs.String("size", "", "target size (e.g. 8M)")
	outDir := fs.String("o", "", "output directory")
	jobs := fs.Int("j", min(4, max(1, runtime.NumCPU()/4)), "parallel jobs")
	noVerify := fs.Bool("no-verify", false, "skip VMAF")
	assumeYes := fs.Bool("y", false, "assume yes / skip wizard")
	showVer := fs.Bool("version", false, "print version")
	if err := fs.Parse(splitArgs(args)); err != nil {
		return 2
	}
	if *showVer {
		fmt.Println(version)
		return 0
	}
	files := fs.Args()
	preflight()
	if *q == "" && *size == "" && !*assumeYes && isStdinTTY() {
		wq, wsize, wfiles, ok := wizard(files)
		if !ok {
			return 1
		}
		*q, *size, files = wq, wsize, wfiles
	}
	if *q == "" && *size == "" {
		*q = "good"
	}
	var p preset
	if *size == "" {
		var ok bool
		p, ok = lookupPreset(*q)
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown preset %q (fast|good|best)\n", *q)
			return 2
		}
	} else {
		p, _ = lookupPreset("good")
		if *q != "" {
			var ok bool
			if p, ok = lookupPreset(*q); !ok {
				fmt.Fprintf(os.Stderr, "unknown preset %q\n", *q)
				return 2
			}
		}
	}
	var target int64
	if *size != "" {
		var err error
		if target, err = parseSize(*size); err != nil {
			fmt.Fprintf(os.Stderr, "bad --size: %v\n", err)
			return 2
		}
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "usage: vidc [flags] <file>...")
		fs.Usage()
		return 2
	}
	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "cannot create -o dir: %v\n", err)
			return 1
		}
	}

	type result struct {
		file    string
		stat    *reportStat
		errText string
		failed  bool
		mu      sync.Mutex
	}
	results := make([]result, len(files))
	sem := make(chan struct{}, max(1, *jobs))
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := &results[i]
			r.file = f
			st, err := processOne(f, p, target, *outDir, !*noVerify)
			if err != nil {
				r.mu.Lock()
				r.errText = err.Error()
				r.mu.Unlock()
				r.failed = true
				return
			}
			r.stat = st
		}(i, f)
	}
	wg.Wait()

	emit(os.Stdout, "\n%-40s %10s %10s %8s %8s\n", "file", "before", "after", "saved", "VMAF")
	failed := 0
	for i := range results {
		r := &results[i]
		if r.failed {
			failed++
			emit(os.Stderr, "%s: FAILED: %s\n", r.file, r.errText)
			emit(os.Stdout, "%-40s %10s %10s %8s %8s\n", r.file, "—", "—", "—", "FAIL")
			continue
		}
		if r.stat == nil {
			emit(os.Stdout, "%-40s %10s %10s %8s %8s\n", r.file, "—", "—", "—", "—")
			continue
		}
		emit(os.Stdout, "%-40s %10s %10s %8s %8s\n", r.file, humanSize(r.stat.before), humanSize(r.stat.after), fmt.Sprintf("%.0f%%", r.stat.savedPct), r.stat.vmafStr)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

type reportStat struct {
	before   int64
	after    int64
	savedPct float64
	vmafStr  string
}

func processOne(f string, p preset, target int64, outDir string, verify bool) (*reportStat, error) {
	info, err := probe(f)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(f)
	if err != nil {
		return nil, err
	}
	info.Size = fi.Size()
	if info.Size <= 0 {
		return nil, fmt.Errorf("%s: empty or unreadable file", f)
	}
	var out string
	if outDir != "" {
		ext := filepath.Ext(f)
		stem := strings.TrimSuffix(filepath.Base(f), ext)
		var err error
		out, err = reserveName(outDir, stem, f)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		out, err = nextOutput(f)
		if err != nil {
			return nil, err
		}
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(out)
		}
	}()
	copyAudio := info.HasAudio && strings.ToLower(info.ACodec) == "aac" && info.AChannels <= 2 && info.AChannels > 0
	pl := plan{In: f, Out: out, P: p, Target: target, CopyAudio: copyAudio, Info: info}
	if err := runEncode(pl, os.Stdout); err != nil {
		return nil, err
	}
	of, err := os.Stat(out)
	if err != nil {
		return nil, err
	}
	before, after := info.Size, of.Size()
	saved := 100 * (1 - float64(after)/float64(before))
	vmafStr := "skipped"
	var score float64
	var offset time.Duration
	var ok bool
	if verify {
		score, offset, ok = measureVMAF(f, out, info.Duration)
		if ok {
			vmafStr = fmt.Sprintf("%.2f @ %s", score, formatOffset(offset))
		}
	}
	st := &reportStat{before, after, saved, vmafShort(vmafStr, ok)}
	outMu.Lock()
	fmt.Printf("%s: %s → %s (%.0f%% smaller)", f, humanSize(before), humanSize(after), saved)
	if ok {
		fmt.Printf(", VMAF %s", vmafStr)
	} else if verify {
		fmt.Printf(", VMAF skipped")
	}
	fmt.Printf("\n  → %s\n", out)
	if ok && score < 93 {
		fmt.Printf("WARNING: VMAF %.2f < 93 — quality may be visibly degraded\n", score)
	}
	if after >= int64(float64(before)*0.95) {
		fmt.Printf("WARNING: output is not at least 5%% smaller than input (%s → %s). Likely cause: source is already well compressed. Keeping both files; nothing was deleted.\n", humanSize(before), humanSize(after))
	}
	outMu.Unlock()
	cleanup = false
	return st, nil
}

func vmafShort(s string, ok bool) string {
	if !ok {
		return "—"
	}
	if i := strings.Index(s, " "); i >= 0 {
		return s[:i]
	}
	return s
}

func formatOffset(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func lookupPreset(name string) (preset, bool) {
	for _, p := range presets {
		if p.Name == name {
			return p, true
		}
	}
	return preset{}, false
}

func preflight() {
	missing := ""
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			missing = b
			break
		}
	}
	if missing == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "%s not found in PATH.\n", missing)
	switch runtime.GOOS {
	case "linux":
		fmt.Fprintln(os.Stderr, "Install: sudo apt install ffmpeg  (or: sudo dnf install ffmpeg)")
	case "darwin":
		fmt.Fprintln(os.Stderr, "Install: brew install ffmpeg")
	case "windows":
		fmt.Fprintln(os.Stderr, "Install: winget install Gyan.FFmpeg")
	default:
		fmt.Fprintln(os.Stderr, "Install: https://ffmpeg.org/download.html")
	}
	os.Exit(1)
}

func isStdinTTY() bool { return isTerminal(os.Stdin) }

// isTerminal reports whether f is an interactive terminal.
// os.ModeCharDevice is NOT sufficient: /dev/null and /dev/zero are character
// devices, so that test wrongly reports them as terminals (which made
// `vidc </dev/null` run the wizard and spin on EOF).
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// splitArgs reorders args so flags (and their values) precede file operands.
// The stdlib flag package stops parsing at the first non-flag argument, so without
// this `vidc video.mp4 -q best` would treat "-q" and "best" as input filenames.
// A literal "--" ends flag parsing; everything after it is a file operand.
func splitArgs(args []string) []string {
	// flags that consume the following argument as their value
	takesValue := map[string]bool{
		"-q": true, "--q": true,
		"-size": true, "--size": true,
		"-o": true, "--o": true,
		"-j": true, "--j": true,
	}
	var flags, files []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// keep the token so fs.Parse still terminates flag parsing itself;
			// without it a file like -dash.mp4 would parse as a flag.
			flags = append(flags, "--")
			files = append(files, args[i+1:]...)
			break
		}
		// a lone "-" is a filename, not a flag
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && takesValue[a] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		files = append(files, a)
	}
	return append(flags, files...)
}

func wizard(files []string) (q, size string, out []string, ok bool) {
	in := bufio.NewReader(os.Stdin)
	out = files
	for len(out) == 0 {
		emit(os.Stdout, "video path: ")
		line, err := in.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			// EOF or read error with nothing buffered: nobody is there to answer.
			return "", "", nil, false
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, err := os.Stat(line); err != nil {
			emit(os.Stdout, "not found: %s\n", line)
			continue
		}
		out = []string{line}
	}
	info, err := probe(out[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot probe %s: %v\n", out[0], err)
		return "", "", nil, false
	}
	if fi, err := os.Stat(out[0]); err == nil {
		info.Size = fi.Size()
	}
	if len(out) > 1 {
		n := len(out)
		if n > 10 {
			for _, f := range out[:10] {
				emit(os.Stdout, "  file      %s\n", filepath.Base(f))
			}
			emit(os.Stdout, "  … and %d more\n", n-10)
		} else {
			for _, f := range out {
				emit(os.Stdout, "  file      %s\n", filepath.Base(f))
			}
		}
	}
	rot := ""
	if info.Rotation != 0 {
		rot = fmt.Sprintf("  rotated %d°", info.Rotation)
	}
	emit(os.Stdout, "\n  file      %s\n", filepath.Base(out[0]))
	emit(os.Stdout, "  %dx%d %.2ffps  %s %s  %s%s\n", info.Width, info.Height, info.FPS, info.VCodec, info.Profile, humanSize(info.Size), rot)
	if info.HasAudio {
		ab := fmt.Sprintf("%dch", info.AChannels)
		abr := ""
		if info.ABitrate > 0 {
			abr = fmt.Sprintf(" %dk", info.ABitrate/1000)
		}
		emit(os.Stdout, "  audio     %s %s%s\n", info.ACodec, ab, abr)
	} else {
		emit(os.Stdout, "  audio     none\n")
	}
	emit(os.Stdout, "\n  Quality:\n")
	for i, pr := range presets {
		marker := ""
		if pr.Name == "good" {
			marker = "   [Enter]"
		}
		emit(os.Stdout, "  %d) %-5s   ~%-6s %-8s  crf%-3d%s\n", i+1, pr.Name, humanSize(estimate(info, pr)), pr.X264, pr.CRF, marker)
	}
	emit(os.Stdout, "\n  or type a target size (e.g. 8M):\n")
	for {
		emit(os.Stdout, "> ")
		line, err := in.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			// EOF with nothing buffered: stop rather than silently
			// encoding with a default for a user who is not there.
			return "", "", nil, false
		}
		line = strings.TrimSpace(strings.ToLower(line))
		if line == "" {
			return "good", "", out, true
		}
		if line == "1" || line == "fast" {
			return "fast", "", out, true
		}
		if line == "2" || line == "good" {
			return "good", "", out, true
		}
		if line == "3" || line == "best" {
			return "best", "", out, true
		}
		if n, err := parseSize(line); err == nil && n > 0 {
			return "", line, out, true
		}
		emit(os.Stdout, "enter 1/2/3, fast/good/best, a size like 8M, or bare Enter for good\n")
	}
}

func estimate(info *Info, pr preset) int64 {
	bpp := estBPP[pr.CRF]
	dur := info.Duration.Seconds()
	if dur <= 0 {
		dur = 1
	}
	var audioKbps float64 = 128
	if info.HasAudio && info.ABitrate > 0 {
		audioKbps = float64(info.ABitrate) / 1000
	} else if !info.HasAudio {
		audioKbps = 0
	}
	est := (bpp*float64(info.Width)*float64(info.Height)*info.FPS + audioKbps*1000) * dur / 8
	if est < 1<<20 {
		est = 1 << 20
	}
	return int64(est)
}

func humanSize(b int64) string {
	switch {
	case b >= 1<<30:
		return strconv.FormatFloat(float64(b)/(1<<30), 'f', 0, 64) + "GB"
	case b >= 1<<20:
		return strconv.FormatFloat(float64(b)/(1<<20), 'f', 0, 64) + "MB"
	case b >= 1<<10:
		return strconv.FormatFloat(float64(b)/(1<<10), 'f', 0, 64) + "KB"
	default:
		return fmt.Sprintf("%dB", b)
	}
}
