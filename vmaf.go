package main

import (
	"fmt"
	"os/exec"
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

func measureVMAF(orig, enc string, d time.Duration) (float64, time.Duration, bool) {
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
	ss := fmt.Sprintf("%.3f", start.Seconds())
	tt := fmt.Sprintf("%.3f", length.Seconds())
	args := []string{"-hide_banner",
		"-ss", ss, "-t", tt, "-i", enc,
		"-ss", ss, "-t", tt, "-i", orig,
		"-lavfi", "[0:v]setpts=PTS-STARTPTS[d];[1:v]setpts=PTS-STARTPTS[r];[d][r]libvmaf",
		"-f", "null", "-"}
	out, err := exec.Command("ffmpeg", args...).CombinedOutput()
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
