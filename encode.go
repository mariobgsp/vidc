package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type preset struct {
	Name string
	CRF  int
	X264 string
}

var presets = []preset{
	{"fast", 20, "veryfast"},
	{"good", 18, "slow"},
	{"best", 16, "slower"},
}

type plan struct {
	In        string
	Out       string
	P         preset
	Target    int64
	CopyAudio bool
	Info      *Info
}

func buildArgs(pl plan, pass int, passlog string) []string {
	a := []string{"ffmpeg", "-hide_banner", "-y", "-i", pl.In,
		"-map", "0:v:0", "-map", "0:a:0?",
		"-map_metadata", "-1"}
	if pl.Info != nil && pl.Info.CreationTime != "" {
		a = append(a, "-metadata", "creation_time="+pl.Info.CreationTime)
	}
	a = append(a, "-map_chapters", "0",
		"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p")
	if pl.Target > 0 {
		a = append(a, "-b:v", videoBitrate(pl.Target, pl.Info), "-preset", pl.P.X264)
		if pass == 1 {
			a = append(a, "-pass", "1", "-passlogfile", passlog, "-an", "-f", "null", "-")
			return a
		}
		a = append(a, "-pass", "2", "-passlogfile", passlog)
		a = appendAudio(a, pl)
		a = append(a, "-movflags", "+faststart",
			"-progress", "pipe:1", "-nostats", "-loglevel", "error", "--", pl.Out)
		return a
	}
	a = append(a, "-crf", strconv.Itoa(pl.P.CRF), "-preset", pl.P.X264)
	a = appendAudio(a, pl)
	a = append(a, "-movflags", "+faststart",
		"-progress", "pipe:1", "-nostats", "-loglevel", "error", "--", pl.Out)
	return a
}

func appendAudio(a []string, pl plan) []string {
	if pl.Info != nil && !pl.Info.HasAudio {
		return a
	}
	if pl.CopyAudio {
		return append(a, "-c:a", "copy")
	}
	return append(a, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
}

func videoBitrate(target int64, info *Info) string {
	budget := int64(float64(target) * 0.98)
	var audioKbps int64
	if info != nil && info.HasAudio {
		if info.ABitrate > 0 {
			audioKbps = int64(info.ABitrate) / 1000
		} else {
			audioKbps = 128
		}
	}
	dur := 1.0
	if info != nil && info.Duration > 0 {
		dur = info.Duration.Seconds()
	}
	vbits := budget*8 - int64(float64(audioKbps)*1000*dur)
	vbr := int64(float64(vbits) / dur)
	if vbr < 100_000 {
		vbr = 100_000
	}
	return strconv.FormatInt(vbr/1000, 10) + "k"
}

var nameMu sync.Mutex

// nextOutput returns a free output path and atomically creates a placeholder for it,
// so two concurrent jobs can never choose the same name.
func nextOutput(in string) (string, error) {
	dir := filepath.Dir(in)
	ext := filepath.Ext(in)
	stem := strings.TrimSuffix(filepath.Base(in), ext)
	return reserveName(dir, stem, in)
}

func candidateNames(dir, stem string) []string {
	names := make([]string, 0, 1000)
	names = append(names, filepath.Join(dir, stem+".min.mp4"))
	for i := 2; i <= 1000; i++ {
		names = append(names, filepath.Join(dir, stem+fmt.Sprintf(".min-%d.mp4", i)))
	}
	return names
}

func reserveName(dir, stem, in string) (string, error) {
	nameMu.Lock()
	defer nameMu.Unlock()
	for _, cand := range candidateNames(dir, stem) {
		f, err := os.OpenFile(cand, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return cand, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("%s: no free output name", in)
}

func runEncode(pl plan, logw io.Writer) error {
	if pl.Target > 0 {
		tmp, err := os.MkdirTemp("", "vidc-pass")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		passlog := filepath.Join(tmp, "pass")
		p1 := buildArgs(pl, 1, passlog)
		var b1 bytes.Buffer
		c1 := exec.Command(p1[0], p1[1:]...)
		c1.Stderr = &b1
		if err := c1.Run(); err != nil {
			return fmt.Errorf("pass 1 failed: %w\n%s", err, b1.String())
		}
		p2 := buildArgs(pl, 2, passlog)
		return runWithProgress(p2, pl.Info.Duration, logw, filepath.Base(pl.In))
	}
	return runWithProgress(buildArgs(pl, 0, ""), pl.Info.Duration, logw, filepath.Base(pl.In))
}

func runWithProgress(argv []string, total time.Duration, logw io.Writer, label string) error {
	if logw == nil {
		logw = os.Stdout
	}
	c := exec.Command(argv[0], argv[1:]...)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	var errout bytes.Buffer
	c.Stderr = &errout
	if err := c.Start(); err != nil {
		return err
	}
	tty := isTTY()
	var outUs int64
	var speed float64
	lastPrint := time.Now().Add(-10 * time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			line := sc.Text()
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			changed := false
			switch k {
			case "out_time_us":
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					outUs = n
					changed = true
				}
			// ponytail: ffmpeg's out_time_ms is actually microseconds (verified on 9.0.1);
			// out_time_us is authoritative, so ignore out_time_ms entirely.
			case "out_time":
				if us, ok := parseOutTime(v); ok {
					outUs = us
					changed = true
				}
			case "speed":
				if s, ok := parseSpeed(v); ok {
					speed = s
					changed = true
				}
			case "progress":
				if v == "end" {
					renderProgress(logw, tty, total, int64(total/time.Microsecond), speed, true, label)
					return
				}
			}
			if !changed {
				continue
			}
			now := time.Now()
			if tty {
				renderProgress(logw, true, total, outUs, speed, false, label)
			} else if now.Sub(lastPrint) >= 5*time.Second {
				lastPrint = now
				renderProgress(logw, false, total, outUs, speed, false, label)
			}
		}
	}()
	err = c.Wait()
	<-done
	if err != nil {
		return fmt.Errorf("ffmpeg failed: %w\n%s", err, errout.String())
	}
	if tty {
		outMu.Lock()
		fmt.Fprintln(logw)
		outMu.Unlock()
	}
	return nil
}

func renderProgress(w io.Writer, tty bool, total time.Duration, outUs int64, speed float64, final bool, label string) {
	if total <= 0 {
		return
	}
	pct := float64(outUs) / float64(total/time.Microsecond) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 || final {
		pct = 100
	}
	if tty {
		eta := "—"
		if speed > 0 && !final {
			rem := (float64(total/time.Microsecond) - float64(outUs)) / 1e6 / speed
			if rem < 0 {
				rem = 0
			}
			eta = (time.Duration(rem) * time.Second).Round(time.Second).String()
		}
		outMu.Lock()
		defer outMu.Unlock()
		fmt.Fprintf(w, "\r[%s] %.1f%%  %0.2fx  ETA %s\033[K", label, pct, speed, eta)
	} else {
		eta := "—"
		if speed > 0 && !final {
			rem := (float64(total/time.Microsecond) - float64(outUs)) / 1e6 / speed
			if rem < 0 {
				rem = 0
			}
			eta = (time.Duration(rem) * time.Second).Round(time.Second).String()
		}
		fmt.Fprintf(w, "[%s] progress: %.1f%% speed=%.2fx eta=%s\n", label, pct, speed, eta)
	}
}

func parseOutTime(s string) (int64, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	sec, err3 := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return int64((float64(h*3600+m*60) + sec) * 1e6), true
}

func parseSpeed(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(s, "x"))
	if s == "" || s == "N/A" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// isTTY reports whether stdout is an interactive terminal.
// See isTerminal in main.go for why os.ModeCharDevice is not a valid test.
func isTTY() bool { return isTerminal(os.Stdout) }

func parseSize(s string) (int64, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	t = strings.TrimSuffix(t, "b")
	t = strings.TrimSpace(t)
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	num := t
	switch {
	case strings.HasSuffix(t, "g"):
		mult = 1 << 30
		num = t[:len(t)-1]
	case strings.HasSuffix(t, "m"):
		mult = 1 << 20
		num = t[:len(t)-1]
	case strings.HasSuffix(t, "k"):
		mult = 1 << 10
		num = t[:len(t)-1]
	}
	num = strings.TrimSpace(num)
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}
