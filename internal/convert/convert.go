package convert

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"s3-webx/internal/config"
)

type Converter struct{}

// thumbnail applies the EXIF orientation itself; --size down keeps small images at their size.
// "[n=-1]" loads every frame, so a GIF stays animated.
func (Converter) Image(
	ctx context.Context,
	in string,
	out string,
	opts config.Image,
) error {
	input := in
	if strings.EqualFold(filepath.Ext(in), ".gif") {
		input += "[n=-1]"
	}
	edge := strconv.Itoa(opts.MaxEdge)
	if err := run(
		ctx,
		"vips",
		"thumbnail",
		input,
		fmt.Sprintf("%s[Q=%d,strip]", out, opts.Quality),
		edge,
		"--height", edge,
		"--size", "down",
	); err != nil {
		return err
	}
	return stripWebPMetadata(out)
}

// webpsave in vips 8.14 keeps the EXIF chunk despite "strip", so camera data and GPS are removed here.
func stripWebPMetadata(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return fmt.Errorf("%s is not a WebP file", path)
	}
	var out bytes.Buffer
	out.Write(data[:12])
	for i := 12; i < len(data); {
		if i+8 > len(data) {
			return fmt.Errorf("%s: truncated chunk header", path)
		}
		fourCC := string(data[i : i+4])
		size := int(binary.LittleEndian.Uint32(data[i+4 : i+8]))
		end := i + 8 + size + size%2
		if end > len(data) {
			return fmt.Errorf("%s: truncated %s chunk", path, fourCC)
		}
		chunk := data[i:end]
		i = end
		switch fourCC {
		case "EXIF", "XMP ":
			continue
		case "VP8X":
			chunk = bytes.Clone(chunk)
			chunk[8] &^= 0x08 | 0x04
		}
		out.Write(chunk)
	}
	result := out.Bytes()
	binary.LittleEndian.PutUint32(result[4:8], uint32(len(result)-8))
	return os.WriteFile(path, result, 0o644)
}

// The scale expressions run after ffmpeg's autorotation, so a portrait phone video has its
// width as the shorter edge.
func scaleFilter(maxHeight int) string {
	h := strconv.Itoa(maxHeight)
	return "scale=" +
		"w='if(lte(iw,ih),trunc(min(iw," + h + ")/2)*2,-2)':" +
		"h='if(lte(iw,ih),-2,trunc(min(ih," + h + ")/2)*2)'"
}

func videoArgs(in, out string, opts config.Video) []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-nostats", "-y",
		"-i", in,
		"-map", "0:v:0", "-map", "0:a:0?", "-map_metadata", "-1",
		"-vf", scaleFilter(opts.MaxHeight),
		"-pix_fmt", "yuv420p",
		"-c:v", "libvpx-vp9", "-crf", strconv.Itoa(opts.CRF), "-b:v", "0",
		"-row-mt", "1", "-deadline", "good", "-cpu-used", "2",
		"-c:a", "libopus", "-b:a", opts.AudioBitrate,
		"-progress", "pipe:1",
		"-f", "webm", out,
	}
}

// Video reports progress in percent; ffmpeg is killed when ctx ends.
func (Converter) Video(
	ctx context.Context,
	in string,
	out string,
	opts config.Video,
	progress func(percent float64),
) error {
	duration, err := probeDuration(ctx, in)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", videoArgs(in, out, opts)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	readProgress(stdout, duration, progress)
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	progress(100)
	return nil
}

func readProgress(r io.Reader, durationSeconds float64, progress func(float64)) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		value, ok := strings.CutPrefix(scanner.Text(), "out_time_us=")
		if !ok || durationSeconds <= 0 {
			continue
		}
		us, err := strconv.ParseFloat(value, 64)
		if err != nil {
			continue
		}
		progress(min(max(us/1e6/durationSeconds*100, 0), 100))
	}
}

func probeDuration(ctx context.Context, in string) (float64, error) {
	cmd := exec.CommandContext(
		ctx,
		"ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		in,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		// A stream without a known duration still converts, only without progress.
		return 0, nil
	}
	return duration, nil
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("%s not installed: %w", name, err)
		}
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
