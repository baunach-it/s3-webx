package convert

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"s3-webx/internal/config"
)

func requireBinaries(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not installed", name)
		}
	}
}

func output(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func noise(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x * 7), uint8(y * 13), uint8(x * y), 255})
		}
	}
	return img
}

// withExif inserts an APP1 segment with Make "SecretCam" and the given orientation after SOI.
func withExif(jpg []byte, orientation uint16) []byte {
	camera := []byte("SecretCam\x00")
	var tiff bytes.Buffer
	be := binary.BigEndian
	tiff.WriteString("MM\x00\x2a")
	binary.Write(&tiff, be, uint32(8))
	binary.Write(&tiff, be, uint16(2))
	binary.Write(&tiff, be, []uint16{0x010F, 2})
	binary.Write(&tiff, be, uint32(len(camera)))
	binary.Write(&tiff, be, uint32(8+2+2*12+4))
	binary.Write(&tiff, be, []uint16{0x0112, 3})
	binary.Write(&tiff, be, uint32(1))
	binary.Write(&tiff, be, []uint16{orientation, 0})
	binary.Write(&tiff, be, uint32(0))
	tiff.Write(camera)
	app1 := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var out bytes.Buffer
	out.Write(jpg[:2])
	out.Write([]byte{0xFF, 0xE1})
	binary.Write(&out, be, uint16(len(app1)+2))
	out.Write(app1)
	out.Write(jpg[2:])
	return out.Bytes()
}

func imageOptions(maxEdge int) config.Image {
	return config.Image{MaxEdge: maxEdge, Quality: 80}
}

func dimensions(t *testing.T, path string) string {
	return output(t, "vipsheader", "-f", "width", path) + "x" + output(t, "vipsheader", "-f", "height", path)
}

func TestImageRotatesShrinksAndStrips(t *testing.T) {
	requireBinaries(t, "vips", "vipsheader")
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, noise(400, 300), &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "source.jpg")
	os.WriteFile(in, withExif(buf.Bytes(), 6), 0o644)
	out := filepath.Join(dir, "out.webp")

	if err := (Converter{}).Image(context.Background(), in, out, imageOptions(100)); err != nil {
		t.Fatal(err)
	}
	if got := dimensions(t, out); got != "75x100" {
		t.Errorf("rotated and shrunk size: %s", got)
	}
	data, _ := os.ReadFile(out)
	if bytes.Contains(data, []byte("SecretCam")) || bytes.Contains(data, []byte("EXIF")) {
		t.Error("output still carries EXIF")
	}
}

func TestImageNeverEnlarges(t *testing.T) {
	requireBinaries(t, "vips", "vipsheader")
	dir := t.TempDir()
	var buf bytes.Buffer
	jpeg.Encode(&buf, noise(120, 80), nil)
	in := filepath.Join(dir, "source.JPG")
	os.WriteFile(in, buf.Bytes(), 0o644)
	out := filepath.Join(dir, "out.webp")

	if err := (Converter{}).Image(context.Background(), in, out, imageOptions(1600)); err != nil {
		t.Fatal(err)
	}
	if got := dimensions(t, out); got != "120x80" {
		t.Errorf("size: %s", got)
	}
}

func TestGifStaysAnimated(t *testing.T) {
	requireBinaries(t, "vips", "vipsheader")
	dir := t.TempDir()
	anim := &gif.GIF{}
	for i := range 5 {
		frame := image.NewPaletted(image.Rect(0, 0, 64, 48), palette.Plan9)
		for x := range 64 {
			frame.SetColorIndex(x, (x+i*10)%48, uint8(i*40))
		}
		anim.Image = append(anim.Image, frame)
		anim.Delay = append(anim.Delay, 10)
	}
	in := filepath.Join(dir, "source.gif")
	f, _ := os.Create(in)
	gif.EncodeAll(f, anim)
	f.Close()
	out := filepath.Join(dir, "out.webp")

	if err := (Converter{}).Image(context.Background(), in, out, imageOptions(1600)); err != nil {
		t.Fatal(err)
	}
	if pages := output(t, "vipsheader", "-f", "n-pages", out); pages != "5" {
		t.Errorf("pages: %s", pages)
	}
}

func testVideo(
	t *testing.T,
	dir string,
	name string,
	size string,
	withAudio bool,
) string {
	t.Helper()
	path := filepath.Join(dir, name)
	args := []string{"-nostdin", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=25"}
	if withAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-c:a", "aac")
	}
	args = append(args, "-t", "2", "-c:v", "libx264", "-pix_fmt", "yuv420p", path)
	output(t, "ffmpeg", args...)
	return path
}

func streams(t *testing.T, path, selector string) string {
	return output(t, "ffprobe", "-v", "error", "-select_streams", selector,
		"-show_entries", "stream=codec_name,width,height", "-of", "csv=p=0", path)
}

func TestVideoScalesShorterEdgeAndKeepsAudio(t *testing.T) {
	requireBinaries(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	opts := config.Video{MaxHeight: 360, CRF: 40, AudioBitrate: "64k"}
	tests := []struct {
		name      string
		size      string
		audio     bool
		wantVideo string
		wantAudio string
	}{
		{"landscape.mp4", "1280x720", true, "vp9,640,360", "opus"},
		{"portrait.mov", "720x1280", false, "vp9,360,640", ""},
		{"odd.m4v", "321x181", false, "vp9,320,180", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := testVideo(t, dir, tt.name, tt.size, tt.audio)
			out := in + ".webm"
			var seen []float64
			err := (Converter{}).Video(context.Background(), in, out, opts, func(p float64) { seen = append(seen, p) })
			if err != nil {
				t.Fatal(err)
			}
			if got := streams(t, out, "v"); got != tt.wantVideo {
				t.Errorf("video stream: %s", got)
			}
			if got := streams(t, out, "a"); got != tt.wantAudio {
				t.Errorf("audio stream: %q", got)
			}
			if len(seen) < 2 || seen[len(seen)-1] != 100 {
				t.Errorf("progress: %v", seen)
			}
		})
	}
}

func TestVideoStopsOnCancel(t *testing.T) {
	requireBinaries(t, "ffmpeg", "ffprobe")
	dir := t.TempDir()
	in := testVideo(t, dir, "long.mp4", "1920x1080", false)
	ctx, cancel := context.WithCancel(context.Background())
	opts := config.Video{MaxHeight: 1080, CRF: 10, AudioBitrate: "96k"}
	started := time.Now()
	err := (Converter{}).Video(ctx, in, in+".webm", opts, func(float64) { cancel() })
	if err != context.Canceled {
		t.Errorf("want context.Canceled, got %v", err)
	}
	if time.Since(started) > 10*time.Second {
		t.Error("ffmpeg kept running after cancel")
	}
}

func TestStripWebPMetadataDropsExifAndFlag(t *testing.T) {
	chunk := func(fourCC string, payload []byte) []byte {
		b := append([]byte(fourCC), 0, 0, 0, 0)
		binary.LittleEndian.PutUint32(b[4:], uint32(len(payload)))
		b = append(b, payload...)
		if len(payload)%2 == 1 {
			b = append(b, 0)
		}
		return b
	}
	vp8x := make([]byte, 10)
	vp8x[0] = 0x08 | 0x04 | 0x10
	body := append([]byte("WEBP"), chunk("VP8X", vp8x)...)
	body = append(body, chunk("EXIF", []byte("SecretCam"))...)
	body = append(body, chunk("VP8 ", []byte{1, 2, 3, 4})...)
	file := append([]byte("RIFF\x00\x00\x00\x00"), body...)
	path := filepath.Join(t.TempDir(), "x.webp")
	os.WriteFile(path, file, 0o644)

	if err := stripWebPMetadata(path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := append([]byte("RIFF\x00\x00\x00\x00WEBP"), chunk("VP8X", append([]byte{0x10}, vp8x[1:]...))...)
	want = append(want, chunk("VP8 ", []byte{1, 2, 3, 4})...)
	binary.LittleEndian.PutUint32(want[4:], uint32(len(want)-8))
	if !bytes.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestReadProgress(t *testing.T) {
	var seen []float64
	readProgress(strings.NewReader("frame=1\nout_time_us=500000\nout_time_us=N/A\nout_time_us=3000000\nprogress=end\n"), 2, func(p float64) {
		seen = append(seen, p)
	})
	if len(seen) != 2 || seen[0] != 25 || seen[1] != 100 {
		t.Errorf("progress: %v", seen)
	}
}
