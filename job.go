package main

// job.go — the shared encode runner used by both the CLI and the GUI.
//
// runJob is the single implementation of the exec+parse+kill logic: it runs
// one encode (CRF or 2-pass), parses ffmpeg's -progress stream, honours ctx
// for cancellation, and captures stderr into the returned error only.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Progress is one progress update for a running encode.
type Progress struct {
	File    string        // base name, for labelling concurrent jobs
	Percent float64       // 0..100
	Speed   float64       // ffmpeg "speed" multiplier, 0 if unknown
	ETA     time.Duration // 0 if unknown
	Done    bool          // true on the final callback for this file
}

// errCancelled is returned when ctx was cancelled.
var errCancelled = errors.New("cancelled")

// runJob executes one encode (CRF or 2-pass) and reports progress via onProgress.
// onProgress may be nil. Honours ctx for cancellation: if ctx is cancelled while
// ffmpeg is encoding, the process is killed, the reserved output pl.Out is deleted,
// and errCancelled is returned. The source file is never touched.
func runJob(ctx context.Context, pl plan, onProgress func(Progress)) error {
	err := runJobInner(ctx, pl, onProgress)
	if errors.Is(err, errCancelled) {
		os.Remove(pl.Out)
	}
	return err
}

func runJobInner(ctx context.Context, pl plan, onProgress func(Progress)) error {
	if pl.Target > 0 {
		tmp, err := os.MkdirTemp("", "vidc-pass")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		passlog := filepath.Join(tmp, "pass")
		p1 := buildArgs(pl, 1, passlog)
		if err := runFFmpeg(ctx, p1, 0, "", "pass 1 failed", nil); err != nil {
			return err
		}
		p2 := buildArgs(pl, 2, passlog)
		return runFFmpeg(ctx, p2, pl.Info.Duration, filepath.Base(pl.In), "ffmpeg failed", onProgress)
	}
	return runFFmpeg(ctx, buildArgs(pl, 0, ""), pl.Info.Duration, filepath.Base(pl.In), "ffmpeg failed", onProgress)
}

// runFFmpeg runs one ffmpeg invocation, parses its -progress stream, and reports
// via onProgress (which may be nil). The callback fires at most a few times per
// progress block: only when a time key or speed was parsed, plus one final
// Done callback on progress=end. stderr is captured and embedded in the returned
// error only — never written anywhere else.
func runFFmpeg(ctx context.Context, argv []string, total time.Duration, label, failPrefix string, onProgress func(Progress)) error {
	c := exec.CommandContext(ctx, toolPath(argv[0]), argv[1:]...)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	var errout bytes.Buffer
	c.Stderr = &errout
	if err := c.Start(); err != nil {
		return err
	}
	var outUs int64
	var speed float64
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
					if onProgress != nil {
						onProgress(Progress{File: label, Percent: 100, Speed: speed, Done: true})
					}
					return
				}
			}
			if !changed || onProgress == nil {
				continue
			}
			pct, eta := progressOf(outUs, total, speed)
			onProgress(Progress{File: label, Percent: pct, Speed: speed, ETA: eta})
		}
	}()
	err = c.Wait()
	<-done
	if wasCancelled(err, ctx) {
		return errCancelled
	}
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", failPrefix, err, errout.String())
	}
	return nil
}

// wasCancelled reports whether a command was cut short by cancellation.
// err == nil means ffmpeg exited successfully: the output is complete, so a
// context cancellation that arrives afterwards must not count. With
// exec.CommandContext a mid-encode cancel always kills the process, so err is
// non-nil whenever a cancel actually interrupted the encode.
func wasCancelled(err error, ctx context.Context) bool {
	return err != nil && ctx.Err() != nil
}

// progressOf converts a parsed out_time into percent and ETA with the exact
// arithmetic the CLI renderer has always used.
func progressOf(outUs int64, total time.Duration, speed float64) (float64, time.Duration) {
	pct := float64(outUs) / float64(total/time.Microsecond) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	var eta time.Duration
	if speed > 0 {
		rem := (float64(total/time.Microsecond) - float64(outUs)) / 1e6 / speed
		if rem < 0 {
			rem = 0
		}
		eta = time.Duration(rem) * time.Second
	}
	return pct, eta
}
