package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	vmafOnce      sync.Once
	vmafCachedVal bool
)

func vmafAvailable() bool {
	vmafOnce.Do(func() {
		out, err := exec.Command("ffmpeg", "-hide_banner", "-filters").CombinedOutput()
		if err != nil {
			vmafCachedVal = false
			return
		}
		vmafCachedVal = strings.Contains(string(out), "libvmaf")
	})
	return vmafCachedVal
}

// vmafFilter builds the lavfi graph. The fast path threads libvmaf and
// subsamples 4x; the full path is the plain filter (bit-exact).
func vmafFilter(full bool) string {
	f := "[0:v]setpts=PTS-STARTPTS[d];[1:v]setpts=PTS-STARTPTS[r];[d][r]libvmaf"
	if !full {
		f += fmt.Sprintf("=n_threads=%d:n_subsample=4", runtime.GOMAXPROCS(0))
	}
	return f
}

func vmafArgs(enc, orig, ss, tt string, exact bool) []string {
	return []string{"-hide_banner",
		"-ss", ss, "-t", tt, "-i", enc,
		"-ss", ss, "-t", tt, "-i", orig,
		"-lavfi", vmafFilter(exact),
		"-f", "null", "-"}
}

func measureVMAF(orig, enc string, d time.Duration, full bool) (float64, time.Duration, bool) {
	if !vmafAvailable() {
		return 0, 0, false
	}
	var start, length time.Duration
	if d < 35*time.Second {
		start, length = 0, d
	} else {
		mid := d/2 - 15*time.Second
		if mid < 0 {
			mid = 0
		}
		start, length = mid, 30*time.Second
	}
	// Short clips: subsampling a handful of frames skews the score, so treat
	// them as an exact pass.
	exact := full || length < 10*time.Second
	ss := fmt.Sprintf("%.3f", start.Seconds())
	tt := fmt.Sprintf("%.3f", length.Seconds())
	out, err := exec.Command("ffmpeg", vmafArgs(enc, orig, ss, tt, exact)...).CombinedOutput()
	if err != nil && !exact {
		out, err = exec.Command("ffmpeg", vmafArgs(enc, orig, ss, tt, true)...).CombinedOutput()
	}
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "VMAF score:"); i >= 0 {
			f, err := strconv.ParseFloat(strings.TrimSpace(line[i+len("VMAF score:"):]), 64)
			if err == nil {
				return f, start, true
			}
		}
	}
	return 0, 0, false
}
