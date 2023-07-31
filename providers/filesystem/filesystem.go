// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package filesystem

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/efficientgo/core/errcapture"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"

	"github.com/thanos-io/objstore"
)

// Config stores the configuration for storing and accessing blobs in filesystem.
type Config struct {
	Directory string `yaml:"directory"`
}

// Bucket implements the objstore.Bucket interfaces against filesystem that binary runs on.
// Methods from Bucket interface are thread-safe. Objects are assumed to be immutable.
// NOTE: It does not follow symbolic links.
type Bucket struct {
	rootDir string
}

// NewBucketFromConfig returns a new filesystem.Bucket from config.
func NewBucketFromConfig(conf []byte) (*Bucket, error) {
	var c Config
	if err := yaml.Unmarshal(conf, &c); err != nil {
		return nil, err
	}
	if c.Directory == "" {
		return nil, errors.New("missing directory for filesystem bucket")
	}
	return NewBucket(c.Directory)
}

// NewBucket returns a new filesystem.Bucket.
func NewBucket(rootDir string) (*Bucket, error) {
	absDir, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	return &Bucket{rootDir: absDir}, nil
}

// Iter calls f for each entry in the given directory. The argument to f is the full
// object name including the prefix of the inspected directory.
func (b *Bucket) Iter(ctx context.Context, dir string, f func(string) error, options ...objstore.IterOption) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	params := objstore.ApplyIterOptions(options...)
	absDir := filepath.Join(b.rootDir, dir)
	info, err := os.Stat(absDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrapf(err, "stat %s", absDir)
	}
	if !info.IsDir() {
		return nil
	}

	files, err := os.ReadDir(absDir)
	if err != nil {
		return err
	}
	for _, file := range files {
		name := filepath.Join(dir, file.Name())

		if file.IsDir() {
			empty, err := isDirEmpty(filepath.Join(absDir, file.Name()))
			if err != nil {
				return err
			}

			if empty {
				// Skip empty directories.
				continue
			}

			name += objstore.DirDelim

			if params.Recursive {
				// Recursively list files in the subdirectory.
				if err := b.Iter(ctx, name, f, options...); err != nil {
					return err
				}

				// The callback f() has already been called for the subdirectory
				// files so we should skip to next filesystem entry.
				continue
			}
		}
		if err := f(name); err != nil {
			return err
		}
	}
	return nil
}

// Get returns a reader for the given object name.
func (b *Bucket) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	return b.GetRange(ctx, name, 0, -1)
}

type rangeReaderCloser struct {
	io.Reader
	f *os.File
}

func (r *rangeReaderCloser) Close() error {
	return r.f.Close()
}

// Attributes returns information about the specified object.
func (b *Bucket) Attributes(ctx context.Context, name string) (objstore.ObjectAttributes, error) {
	if ctx.Err() != nil {
		return objstore.ObjectAttributes{}, ctx.Err()
	}

	file := filepath.Join(b.rootDir, name)
	stat, err := os.Stat(file)
	if err != nil {
		return objstore.ObjectAttributes{}, errors.Wrapf(err, "stat %s", file)
	}

	return objstore.ObjectAttributes{
		Size:         stat.Size(),
		LastModified: stat.ModTime(),
	}, nil
}

// GetRange returns a new range reader for the given object name and range.
func (b *Bucket) GetRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if name == "" {
		return nil, errors.New("object name is empty")
	}

	file := filepath.Join(b.rootDir, name)
	if _, err := os.Stat(file); err != nil {
		return nil, errors.Wrapf(err, "stat %s", file)
	}

	f, err := os.OpenFile(filepath.Clean(file), os.O_RDONLY, 0600)
	if err != nil {
		return nil, err
	}

	if off > 0 {
		_, err := f.Seek(off, 0)
		if err != nil {
			return nil, errors.Wrapf(err, "seek %v", off)
		}
	}

	if length == -1 {
		return f, nil
	}

	return &rangeReaderCloser{Reader: io.LimitReader(f, length), f: f}, nil
}

// Exists checks if the given directory exists in memory.
func (b *Bucket) Exists(ctx context.Context, name string) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	info, err := os.Stat(filepath.Join(b.rootDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, errors.Wrapf(err, "stat %s", filepath.Join(b.rootDir, name))
	}
	return !info.IsDir(), nil
}

// Upload writes the file specified in src to into the memory.
func (b *Bucket) Upload(ctx context.Context, name string, r io.Reader) (err error) {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	file := filepath.Join(b.rootDir, name)
	if err := os.MkdirAll(filepath.Dir(file), os.ModePerm); err != nil {
		return err
	}

	f, err := os.Create(file)
	if err != nil {
		return err
	}
	defer errcapture.Do(&err, f.Close, "close")

	_, err = io.Copy(f, r)
	if err != nil {
		return errors.Wrapf(err, "copy to %s", file)
	}
	return nil
}

func isDirEmpty(name string) (ok bool, err error) {
	f, err := os.Open(filepath.Clean(name))
	if os.IsNotExist(err) {
		// The directory doesn't exist. We don't consider it an error and we treat it like empty.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer errcapture.Do(&err, f.Close, "close dir")

	if _, err = f.Readdir(1); err == io.EOF || os.IsNotExist(err) {
		return true, nil
	}
	return false, err
}

// Delete removes all data prefixed with the dir.
func (b *Bucket) Delete(ctx context.Context, name string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	file := filepath.Join(b.rootDir, name)
	for file != b.rootDir {
		if err := os.RemoveAll(file); err != nil {
			return errors.Wrapf(err, "rm %s", file)
		}
		file = filepath.Dir(file)
		empty, err := isDirEmpty(file)
		if err != nil {
			return err
		}
		if !empty {
			break
		}
	}
	return nil
}

// IsObjNotFoundErr returns true if error means that object is not found. Relevant to Get operations.
func (b *Bucket) IsObjNotFoundErr(err error) bool {
	return os.IsNotExist(errors.Cause(err))
}

// IsAccessDeniedErr returns true if access to object is denied.
func (b *Bucket) IsAccessDeniedErr(_ error) bool {
	return false
}

func (b *Bucket) Close() error { return nil }

// Name returns the bucket name.
func (b *Bucket) Name() string {
	return fmt.Sprintf("fs: %s", b.rootDir)
}
