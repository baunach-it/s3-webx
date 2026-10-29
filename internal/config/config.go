package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const CacheControlInherit = "inherit"

var (
	knownImageFormats = []string{"jpg", "jpeg", "png", "gif", "tif", "tiff"}
	knownVideoFormats = []string{"mp4", "mov", "m4v", "mkv", "avi"}
	audioBitrate      = regexp.MustCompile(`^[1-9][0-9]*k$`)
	envReference      = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

type Config struct {
	Listen   string
	State    string
	Interval time.Duration
	Auth     Auth
	Buckets  []Bucket
}

type Auth struct {
	UIUser     string
	UIPassword string
	APIToken   string
}

type Image struct {
	Formats []string
	MaxEdge int
	Quality int
}

type Video struct {
	Enabled      bool
	Formats      []string
	MaxHeight    int
	CRF          int
	AudioBitrate string
}

type Bucket struct {
	Name         string
	Endpoint     string
	Region       string
	Bucket       string
	Prefix       string
	PathStyle    bool
	AccessKey    string
	SecretKey    string
	Interval     time.Duration
	Image        Image
	Video        Video
	KeepIfLarger bool
	CacheControl string
}

// WorkDir sits next to the state file so temporary files land on the same volume.
func (c Config) WorkDir() string {
	return filepath.Join(filepath.Dir(c.State), "tmp")
}

type fileConfig struct {
	Listen   string         `yaml:"listen"`
	State    string         `yaml:"state"`
	Interval *time.Duration `yaml:"interval"`
	Auth     struct {
		UIUser     string `yaml:"ui_user"`
		UIPassword string `yaml:"ui_password"`
		APIToken   string `yaml:"api_token"`
	} `yaml:"auth"`
	Defaults fileOverrides `yaml:"defaults"`
	Buckets  []fileBucket  `yaml:"buckets"`
}

type fileOverrides struct {
	Image        *fileImage `yaml:"image"`
	Video        *fileVideo `yaml:"video"`
	KeepIfLarger *bool      `yaml:"keep_if_larger"`
	CacheControl *string    `yaml:"cache_control"`
}

type fileImage struct {
	Formats []string `yaml:"formats"`
	MaxEdge *int     `yaml:"max_edge"`
	Quality *int     `yaml:"quality"`
}

type fileVideo struct {
	Enabled      *bool    `yaml:"enabled"`
	Formats      []string `yaml:"formats"`
	MaxHeight    *int     `yaml:"max_height"`
	CRF          *int     `yaml:"crf"`
	AudioBitrate *string  `yaml:"audio_bitrate"`
}

type fileBucket struct {
	Name          string         `yaml:"name"`
	Endpoint      string         `yaml:"endpoint"`
	Region        string         `yaml:"region"`
	Bucket        string         `yaml:"bucket"`
	Prefix        string         `yaml:"prefix"`
	PathStyle     bool           `yaml:"path_style"`
	AccessKey     string         `yaml:"access_key"`
	SecretKey     string         `yaml:"secret_key"`
	Interval      *time.Duration `yaml:"interval"`
	fileOverrides `yaml:",inline"`
}

func Load(path string, lookupEnv func(string) (string, bool)) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(data, lookupEnv)
}

func Parse(data []byte, lookupEnv func(string) (string, bool)) (Config, error) {
	expanded, err := expandEnv(data, lookupEnv)
	if err != nil {
		return Config{}, err
	}
	var file fileConfig
	decoder := yaml.NewDecoder(bytes.NewReader(expanded))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg := build(file)
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Expanding on the parsed node tree instead of the raw text keeps values with YAML syntax
// (":", "#", quotes) from breaking the document.
func expandEnv(data []byte, lookupEnv func(string) (string, bool)) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if root.Kind == 0 {
		return data, nil
	}
	var missing []string
	var walk func(node *yaml.Node)
	walk = func(node *yaml.Node) {
		if node.Kind == yaml.ScalarNode {
			node.Value = envReference.ReplaceAllStringFunc(node.Value, func(ref string) string {
				name := envReference.FindStringSubmatch(ref)[1]
				value, ok := lookupEnv(name)
				if !ok {
					if !slices.Contains(missing, name) {
						missing = append(missing, name)
					}
				}
				return value
			})
		}
		for _, child := range node.Content {
			walk(child)
		}
	}
	walk(&root)
	if len(missing) > 0 {
		return nil, fmt.Errorf("config references unset environment variables: %s", strings.Join(missing, ", "))
	}
	return yaml.Marshal(&root)
}

func build(file fileConfig) Config {
	cfg := Config{
		Listen:   orDefault(file.Listen, ":8080"),
		State:    orDefault(file.State, "/data/state.db"),
		Interval: 15 * time.Minute,
		Auth: Auth{
			UIUser:     file.Auth.UIUser,
			UIPassword: file.Auth.UIPassword,
			APIToken:   file.Auth.APIToken,
		},
	}
	if file.Interval != nil {
		cfg.Interval = *file.Interval
	}
	defaults := Bucket{
		Image: Image{
			Formats: []string{"jpg", "jpeg", "png", "gif"},
			MaxEdge: 1600,
			Quality: 80,
		},
		Video: Video{
			Enabled:      true,
			Formats:      []string{"mp4", "mov", "m4v"},
			MaxHeight:    1080,
			CRF:          33,
			AudioBitrate: "96k",
		},
		KeepIfLarger: true,
		CacheControl: CacheControlInherit,
	}
	defaults.apply(file.Defaults)
	for _, fb := range file.Buckets {
		b := defaults
		b.Image.Formats = slices.Clone(defaults.Image.Formats)
		b.Video.Formats = slices.Clone(defaults.Video.Formats)
		b.Name = fb.Name
		b.Endpoint = fb.Endpoint
		b.Region = orDefault(fb.Region, "auto")
		b.Bucket = fb.Bucket
		b.Prefix = fb.Prefix
		b.PathStyle = fb.PathStyle
		b.AccessKey = fb.AccessKey
		b.SecretKey = fb.SecretKey
		b.Interval = cfg.Interval
		if fb.Interval != nil {
			b.Interval = *fb.Interval
		}
		b.apply(fb.fileOverrides)
		cfg.Buckets = append(cfg.Buckets, b)
	}
	return cfg
}

func (b *Bucket) apply(o fileOverrides) {
	if i := o.Image; i != nil {
		if i.Formats != nil {
			b.Image.Formats = normalizeFormats(i.Formats)
		}
		setIfPresent(&b.Image.MaxEdge, i.MaxEdge)
		setIfPresent(&b.Image.Quality, i.Quality)
	}
	if v := o.Video; v != nil {
		setIfPresent(&b.Video.Enabled, v.Enabled)
		if v.Formats != nil {
			b.Video.Formats = normalizeFormats(v.Formats)
		}
		setIfPresent(&b.Video.MaxHeight, v.MaxHeight)
		setIfPresent(&b.Video.CRF, v.CRF)
		setIfPresent(&b.Video.AudioBitrate, v.AudioBitrate)
	}
	setIfPresent(&b.KeepIfLarger, o.KeepIfLarger)
	setIfPresent(&b.CacheControl, o.CacheControl)
}

func validate(cfg Config) error {
	var problems []string
	if cfg.Interval <= 0 {
		problems = append(problems, "interval must be greater than 0")
	}
	if len(cfg.Buckets) == 0 {
		problems = append(problems, "at least one bucket is required")
	}
	seen := map[string]bool{}
	for i, b := range cfg.Buckets {
		where := fmt.Sprintf("buckets[%d]", i)
		if b.Name == "" {
			problems = append(problems, where+": name is required")
		} else {
			where = fmt.Sprintf("bucket %q", b.Name)
			if seen[b.Name] {
				problems = append(problems, where+": duplicate name")
			}
			seen[b.Name] = true
		}
		if b.Endpoint == "" {
			problems = append(problems, where+": endpoint is required")
		}
		if b.Bucket == "" {
			problems = append(problems, where+": bucket is required")
		}
		if b.AccessKey == "" || b.SecretKey == "" {
			problems = append(problems, where+": access_key and secret_key are required")
		}
		if b.Interval <= 0 {
			problems = append(problems, where+": interval must be greater than 0")
		}
		if b.Image.MaxEdge <= 0 {
			problems = append(problems, where+": image.max_edge must be greater than 0")
		}
		if b.Image.Quality < 1 || b.Image.Quality > 100 {
			problems = append(problems, where+": image.quality must be between 1 and 100")
		}
		if b.Video.MaxHeight <= 0 {
			problems = append(problems, where+": video.max_height must be greater than 0")
		}
		if b.Video.CRF < 0 || b.Video.CRF > 63 {
			problems = append(problems, where+": video.crf must be between 0 and 63")
		}
		if !audioBitrate.MatchString(b.Video.AudioBitrate) {
			problems = append(problems, where+": video.audio_bitrate must look like 96k")
		}
		if b.CacheControl == "" {
			problems = append(problems, where+": cache_control must be inherit or a fixed value")
		}
		for _, f := range b.Image.Formats {
			if !slices.Contains(knownImageFormats, f) {
				problems = append(problems, fmt.Sprintf("%s: unknown image format %q", where, f))
			}
		}
		for _, f := range b.Video.Formats {
			if !slices.Contains(knownVideoFormats, f) {
				problems = append(problems, fmt.Sprintf("%s: unknown video format %q", where, f))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(problems, "; "))
	}
	return nil
}

func normalizeFormats(formats []string) []string {
	out := make([]string, 0, len(formats))
	for _, f := range formats {
		out = append(out, strings.ToLower(strings.TrimPrefix(strings.TrimSpace(f), ".")))
	}
	return out
}

func setIfPresent[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
