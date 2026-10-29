package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

const minimal = `
buckets:
  - name: content
    endpoint: https://example.r2.cloudflarestorage.com
    bucket: content
    access_key: ${ACCESS}
    secret_key: ${SECRET}
`

var minimalEnv = env(map[string]string{"ACCESS": "a", "SECRET": "s"})

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal), minimalEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.State != "/data/state.db" || cfg.Interval != 15*time.Minute {
		t.Errorf("top-level defaults: %+v", cfg)
	}
	if cfg.WorkDir() != "/data/tmp" {
		t.Errorf("work dir: %s", cfg.WorkDir())
	}
	b := cfg.Buckets[0]
	if b.Region != "auto" || b.Interval != 15*time.Minute || b.CacheControl != CacheControlInherit || !b.KeepIfLarger {
		t.Errorf("bucket defaults: %+v", b)
	}
	if !slices.Equal(b.Image.Formats, []string{"jpg", "jpeg", "png", "gif"}) || b.Image.MaxEdge != 1600 || b.Image.Quality != 80 {
		t.Errorf("image defaults: %+v", b.Image)
	}
	if !b.Video.Enabled || !slices.Equal(b.Video.Formats, []string{"mp4", "mov", "m4v"}) ||
		b.Video.MaxHeight != 1080 || b.Video.CRF != 33 || b.Video.AudioBitrate != "96k" {
		t.Errorf("video defaults: %+v", b.Video)
	}
	if b.AccessKey != "a" || b.SecretKey != "s" {
		t.Errorf("secrets not expanded: %q %q", b.AccessKey, b.SecretKey)
	}
}

func TestParseOverridesDefaultsFieldByField(t *testing.T) {
	cfg, err := Parse([]byte(`
interval: 1h
defaults:
  image:
    quality: 70
  video:
    crf: 30
  cache_control: public, max-age=60
buckets:
  - name: one
    endpoint: http://minio:9000
    bucket: one
    access_key: a
    secret_key: s
  - name: two
    endpoint: http://minio:9000
    bucket: two
    access_key: a
    secret_key: s
    interval: 5m
    image:
      max_edge: 800
      formats: [JPG, .Png]
    video:
      enabled: false
    keep_if_larger: false
`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	one, two := cfg.Buckets[0], cfg.Buckets[1]
	if one.Interval != time.Hour || one.Image.Quality != 70 || one.Image.MaxEdge != 1600 || one.Video.CRF != 30 || !one.Video.Enabled || !one.KeepIfLarger {
		t.Errorf("bucket one: %+v", one)
	}
	if one.CacheControl != "public, max-age=60" {
		t.Errorf("cache control: %q", one.CacheControl)
	}
	if two.Interval != 5*time.Minute || two.Image.MaxEdge != 800 || two.Image.Quality != 70 || two.Video.Enabled || two.Video.CRF != 30 || two.KeepIfLarger {
		t.Errorf("bucket two: %+v", two)
	}
	if !slices.Equal(two.Image.Formats, []string{"jpg", "png"}) {
		t.Errorf("formats: %v", two.Image.Formats)
	}
	if !slices.Equal(one.Image.Formats, []string{"jpg", "jpeg", "png", "gif"}) {
		t.Errorf("override leaked into bucket one: %v", one.Image.Formats)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown key", minimal + "    colour: blue\n", "colour"},
		{"unknown top-level key", "workers: 4\n" + minimal, "workers"},
		{"duplicate name", minimal + strings.ReplaceAll(minimal, "buckets:\n", ""), "duplicate name"},
		{"quality out of range", "defaults:\n  image:\n    quality: 101\n" + minimal, "quality"},
		{"crf out of range", "defaults:\n  video:\n    crf: 64\n" + minimal, "crf"},
		{"zero max edge", "defaults:\n  image:\n    max_edge: 0\n" + minimal, "max_edge"},
		{"negative interval", "interval: -1m\n" + minimal, "interval"},
		{"unknown image format", "defaults:\n  image:\n    formats: [bmp]\n" + minimal, `"bmp"`},
		{"unknown video format", "defaults:\n  video:\n    formats: [flv]\n" + minimal, `"flv"`},
		{"no buckets", "listen: :9000\n", "at least one bucket"},
		{"missing endpoint", strings.Replace(minimal, "    endpoint: https://example.r2.cloudflarestorage.com\n", "", 1), "endpoint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml), minimalEnv)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("want error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestParseNamesMissingEnvironmentVariables(t *testing.T) {
	_, err := Parse([]byte(minimal), env(map[string]string{"ACCESS": "a"}))
	if err == nil || !strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "ACCESS") {
		t.Errorf("want error naming only SECRET, got %v", err)
	}
}

func TestParseKeepsYAMLSyntaxInSecrets(t *testing.T) {
	cfg, err := Parse([]byte(minimal), env(map[string]string{"ACCESS": "a: b", "SECRET": "#x'\""}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Buckets[0].AccessKey != "a: b" || cfg.Buckets[0].SecretKey != "#x'\"" {
		t.Errorf("secrets mangled: %q %q", cfg.Buckets[0].AccessKey, cfg.Buckets[0].SecretKey)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	anyValue := func(string) (string, bool) { return "x", true }
	if _, err := Parse(data, anyValue); err != nil {
		t.Fatal(err)
	}
}
