package worker

import (
	"context"
	"errors"
	"os"
	"time"

	"s3-webx/internal/config"
	"s3-webx/internal/store"
)

type upload struct {
	Key          string
	ContentType  string
	CacheControl string
	Size         int64
}

type storeDouble struct {
	objects      []store.Object
	cacheControl map[string]string
	downloads    []string
	uploads      []upload
	cacheLookups []string
	listFailure  error
}

func newStoreDouble() *storeDouble {
	return &storeDouble{cacheControl: map[string]string{}}
}

func (s *storeDouble) givenObject(key string, size int64, modified time.Time) *storeDouble {
	s.objects = append(s.objects, store.Object{Key: key, Size: size, LastModified: modified})
	return s
}

func (s *storeDouble) givenCacheControl(key, value string) *storeDouble {
	s.cacheControl[key] = value
	return s
}

func (s *storeDouble) givenListFailure(err error) *storeDouble {
	s.listFailure = err
	return s
}

func (s *storeDouble) List(context.Context) ([]store.Object, error) {
	return s.objects, s.listFailure
}

func (s *storeDouble) Download(_ context.Context, key, path string) error {
	s.downloads = append(s.downloads, key)
	return os.WriteFile(path, []byte("original"), 0o644)
}

func (s *storeDouble) Upload(
	_ context.Context,
	key string,
	path string,
	contentType string,
	cacheControl string,
) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	s.uploads = append(s.uploads, upload{
		Key:          key,
		ContentType:  contentType,
		CacheControl: cacheControl,
		Size:         info.Size(),
	})
	return nil
}

func (s *storeDouble) CacheControl(_ context.Context, key string) (string, error) {
	s.cacheLookups = append(s.cacheLookups, key)
	return s.cacheControl[key], nil
}

type conversion struct {
	In    string
	Out   string
	Image config.Image
	Video config.Video
}

type converterDouble struct {
	outputSize    int64
	failure       error
	cancelOnStart context.CancelFunc
	during        func()
	images        []conversion
	videos        []conversion
}

func newConverterDouble() *converterDouble {
	return &converterDouble{outputSize: 10}
}

func (c *converterDouble) givenOutputSize(n int64) *converterDouble {
	c.outputSize = n
	return c
}

func (c *converterDouble) givenFailure(err error) *converterDouble {
	c.failure = err
	return c
}

// givenShutdownDuringConversion cancels the worker's context the way SIGTERM would mid-conversion.
func (c *converterDouble) givenShutdownDuringConversion(cancel context.CancelFunc) *converterDouble {
	c.cancelOnStart = cancel
	return c
}

func (c *converterDouble) givenDuringConversion(f func()) *converterDouble {
	c.during = f
	return c
}

func (c *converterDouble) produce(ctx context.Context, in, out string) error {
	if _, err := os.Stat(in); err != nil {
		return errors.New("input was not downloaded")
	}
	if c.during != nil {
		c.during()
	}
	if c.cancelOnStart != nil {
		c.cancelOnStart()
		return ctx.Err()
	}
	if c.failure != nil {
		return c.failure
	}
	return os.WriteFile(out, make([]byte, c.outputSize), 0o644)
}

func (c *converterDouble) Image(
	ctx context.Context,
	in string,
	out string,
	opts config.Image,
) error {
	c.images = append(c.images, conversion{In: in, Out: out, Image: opts})
	return c.produce(ctx, in, out)
}

func (c *converterDouble) Video(
	ctx context.Context,
	in string,
	out string,
	opts config.Video,
	progress func(float64),
) error {
	c.videos = append(c.videos, conversion{In: in, Out: out, Video: opts})
	progress(50)
	return c.produce(ctx, in, out)
}
