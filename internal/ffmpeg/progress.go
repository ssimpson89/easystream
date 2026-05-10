package ffmpeg

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
)

type Progress struct {
	Frame       int       `json:"frame"`
	FPS         float64   `json:"fps"`
	BitrateKbps float64   `json:"bitrateKbps"`
	Speed       string    `json:"speed"`
	Dropped     int       `json:"dropped"`
	UpdatedAt   time.Time `json:"updatedAt"`
	RawStatus   string    `json:"rawStatus"`
}

func ParseProgress(r io.Reader, emit func(Progress)) error {
	scanner := bufio.NewScanner(r)
	current := Progress{}
	for scanner.Scan() {
		line := scanner.Text()
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "frame":
			current.Frame, _ = strconv.Atoi(value)
		case "fps":
			current.FPS, _ = strconv.ParseFloat(value, 64)
		case "bitrate":
			current.BitrateKbps = parseKbits(value)
		case "speed":
			current.Speed = value
		case "drop_frames":
			current.Dropped, _ = strconv.Atoi(value)
		case "progress":
			current.RawStatus = value
			current.UpdatedAt = time.Now().UTC()
			emit(current)
			if value == "end" {
				current = Progress{}
			}
		}
	}
	return scanner.Err()
}

func parseKbits(value string) float64 {
	trimmed := strings.TrimSpace(strings.TrimSuffix(value, "kbits/s"))
	trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "kbit/s"))
	out, _ := strconv.ParseFloat(trimmed, 64)
	return out
}
