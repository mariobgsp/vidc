package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Info struct {
	Path         string
	Size         int64
	Duration     time.Duration
	Width        int
	Height       int
	FPS          float64
	VCodec       string
	Profile      string
	PixFmt       string
	ACodec       string
	AChannels    int
	ABitrate     int
	CreationTime string
	Rotation     int
	HasVideo     bool
	HasAudio     bool
}

type ffprobeOut struct {
	Format  ffFormat   `json:"format"`
	Streams []ffStream `json:"streams"`
}

type ffFormat struct {
	Duration string            `json:"duration"`
	BitRate  string            `json:"bit_rate"`
	Tags     map[string]string `json:"tags"`
}

type ffStream struct {
	CodecType    string            `json:"codec_type"`
	CodecName    string            `json:"codec_name"`
	Profile      string            `json:"profile"`
	PixFmt       string            `json:"pix_fmt"`
	Width        int               `json:"width"`
	Height       int               `json:"height"`
	RFrameRate   string            `json:"r_frame_rate"`
	AvgFrameRate string            `json:"avg_frame_rate"`
	Duration     string            `json:"duration"`
	BitRate      string            `json:"bit_rate"`
	Channels     int               `json:"channels"`
	Tags         map[string]string `json:"tags"`
	SideData     []ffSideData      `json:"side_data_list"`
}

type ffSideData struct {
	Rotation *float64 `json:"rotation"`
}

func probe(path string) (*Info, error) {
	cmd := exec.Command(toolPath("ffprobe"), "-v", "error", "-print_format", "json", "-show_format", "-show_streams", "--", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	var p ffprobeOut
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("%s: bad ffprobe output: %w", path, err)
	}
	var vs, as *ffStream
	for i := range p.Streams {
		s := &p.Streams[i]
		if vs == nil && s.CodecType == "video" {
			vs = s
		}
		if as == nil && s.CodecType == "audio" {
			as = s
		}
	}
	if vs == nil {
		return nil, fmt.Errorf("%s: no video stream", path)
	}
	info := &Info{Path: path, HasVideo: true}
	info.VCodec = vs.CodecName
	info.Profile = strings.ToLower(vs.Profile)
	info.PixFmt = vs.PixFmt
	info.Width = vs.Width
	info.Height = vs.Height
	info.FPS = parseFrac(vs.RFrameRate)
	if info.FPS == 0 {
		info.FPS = parseFrac(vs.AvgFrameRate)
	}
	for _, sd := range vs.SideData {
		if sd.Rotation != nil {
			info.Rotation = int(*sd.Rotation)
		}
	}
	dur := parseDur(p.Format.Duration)
	if dur == 0 {
		dur = parseDur(vs.Duration)
	}
	if dur == 0 {
		return nil, fmt.Errorf("%s: unknown duration", path)
	}
	info.Duration = dur
	if ct := p.Format.Tags["creation_time"]; ct != "" {
		info.CreationTime = ct
	} else if vs.Tags["creation_time"] != "" {
		info.CreationTime = vs.Tags["creation_time"]
	}
	if as != nil {
		info.HasAudio = true
		info.ACodec = as.CodecName
		info.AChannels = as.Channels
		if b, err := strconv.Atoi(as.BitRate); err == nil {
			info.ABitrate = b
		} else if b, err := strconv.Atoi(p.Format.BitRate); err == nil {
			info.ABitrate = b
		}
	}
	return info, nil
}

func parseFrac(s string) float64 {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0
	}
	n, err1 := strconv.ParseFloat(parts[0], 64)
	d, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}

func parseDur(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}
