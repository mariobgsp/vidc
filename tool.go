package main

// tool.go — external-tool resolution.
//
// A bundled ffmpeg/ffprobe sitting next to the vidc executable wins (that is how
// the release packages ship, including inside an AppImage); otherwise we fall back
// to PATH so a system ffmpeg still works.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// toolPath resolves an external tool. A copy sitting next to the vidc executable
// wins (that is how the bundled ffmpeg ships, including inside an AppImage);
// otherwise fall back to PATH so a system ffmpeg still works. If neither has it,
// the bare name is returned so the existing "not found" error path fires.
func toolPath(name string) string {
	if exe, err := os.Executable(); err == nil {
		if p, ok := toolPathIn(filepath.Dir(exe), name); ok {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

// toolPathIn looks for an executable name inside one directory: <dir>/<name>
// (+ .exe on Windows). It reports false for missing files, directories, and
// (outside Windows, which has no exec bit) non-executable files.
func toolPathIn(dir, name string) (string, bool) {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cand := filepath.Join(dir, name)
	fi, err := os.Stat(cand)
	if err != nil || fi.IsDir() {
		return "", false
	}
	if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
		return "", false
	}
	return cand, true
}
