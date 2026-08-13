package main

import (
	"io"

	"sharebridge/agent/internal/cloudwebdav"
)

type benchStorage struct{ size int64 }

func (b benchStorage) ListFiles(subpath string) ([]cloudwebdav.FileInfo, error) {
	return []cloudwebdav.FileInfo{{Name: "bench.bin", Size: b.size}}, nil
}

func (b benchStorage) GetFile(filePath string, w io.Writer) (int64, error) {
	buf := make([]byte, 64*1024)
	var written int64
	for written < b.size {
		n := int64(len(buf))
		if rem := b.size - written; rem < n {
			n = rem
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

func (b benchStorage) GetSHA1(subpath string) string { return "" }
