package config

import (
	"bytes"
	"io/fs"
	"time"
)

// Check validates a candidate configuration: files maps each file's path under the configuration root
// to its content. It returns the violations Load would report for the same content, so none means the
// candidate loads. Nothing is written and no candidate file is read from disk; a file the candidate does
// not name does not exist. Host files the configuration names (see reference) are stat'ed under root.
func Check(root string, files map[string]string) []Violation {
	src := make(mapFS, len(files))
	for p, content := range files {
		src[p] = []byte(content)
	}
	_, vs := load(src, root)
	return vs
}

// mapFS is a read-only file system over in-memory files keyed by slash-separated path.
type mapFS map[string][]byte

func (m mapFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	b, ok := m[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &mapFile{name: name, Reader: bytes.NewReader(b)}, nil
}

type mapFile struct {
	name string
	*bytes.Reader
}

func (f *mapFile) Stat() (fs.FileInfo, error) {
	return mapInfo{name: f.name, size: f.Size()}, nil
}

func (f *mapFile) Close() error { return nil }

type mapInfo struct {
	name string
	size int64
}

func (i mapInfo) Name() string       { return i.name }
func (i mapInfo) Size() int64        { return i.size }
func (i mapInfo) Mode() fs.FileMode  { return 0o444 }
func (i mapInfo) ModTime() time.Time { return time.Time{} }
func (i mapInfo) IsDir() bool        { return false }
func (i mapInfo) Sys() any           { return nil }
