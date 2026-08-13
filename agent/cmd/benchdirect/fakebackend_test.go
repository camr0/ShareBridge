package main

import (
	"bytes"
	"testing"
)

func TestGetFileRejectsUnknownPath(t *testing.T) {
	s := benchStorage{size: 1024}
	if _, err := s.GetFile("other.bin", &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for non-bench.bin path")
	}
}

func TestGetFileStreamsBenchBin(t *testing.T) {
	s := benchStorage{size: 1024}
	var buf bytes.Buffer
	n, err := s.GetFile("bench.bin", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1024 || int64(buf.Len()) != 1024 {
		t.Fatalf("got %d bytes (%d written), want 1024", buf.Len(), n)
	}
}
