// Copyright (c) 2026, go-volumes
// SPDX-License-Identifier: BSD-3-Clause

package pool

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

var errInjected = errors.New("injected backing failure")

// memBacking is an in-memory Backing with per-operation failure injection, used
// to drive the CreateWith / OpenWith error paths that a real file never hits.
type memBacking struct {
	data         []byte
	failReadAt   bool
	failWriteAt  bool
	failTruncate bool
	failSync     bool
	closed       bool
}

var _ Backing = (*memBacking)(nil)

func (m *memBacking) ReadAt(p []byte, off int64) (int, error) {
	if m.failReadAt {
		return 0, errInjected
	}
	if off < 0 || off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memBacking) WriteAt(p []byte, off int64) (int, error) {
	if m.failWriteAt {
		return 0, errInjected
	}
	if end := off + int64(len(p)); end > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}
	copy(m.data[off:], p)
	return len(p), nil
}

func (m *memBacking) Truncate(size int64) error {
	if m.failTruncate {
		return errInjected
	}
	if size <= int64(len(m.data)) {
		m.data = m.data[:size]
	} else {
		m.data = append(m.data, make([]byte, size-int64(len(m.data)))...)
	}
	return nil
}

func (m *memBacking) Sync() error {
	if m.failSync {
		return errInjected
	}
	return nil
}

func (m *memBacking) Close() error {
	m.closed = true
	return nil
}

// TestCreateWithRoundTrip proves the seam works on a non-file backing: create a
// pool on memory, write through a volume, then reopen the same bytes.
func TestCreateWithRoundTrip(t *testing.T) {
	mb := &memBacking{}
	p, err := CreateWith(mb, 64*1024, 512)
	if err != nil {
		t.Fatalf("CreateWith: %v", err)
	}
	v, err := p.CreateVolume("v", 4096)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	want := []byte("the weft over the warp")
	if _, err := v.WriteAt(want, 1000); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !mb.closed {
		t.Fatal("Close did not close the backing")
	}

	// Reopen the very same bytes on a fresh backing.
	mb2 := &memBacking{data: mb.data}
	p2, err := OpenWith(mb2)
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	defer p2.Close()
	v2, err := p2.OpenVolume("v")
	if err != nil {
		t.Fatalf("OpenVolume: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := v2.ReadAt(got, 1000); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("round-trip: got %q, want %q", got, want)
	}
}

func TestCreateWithBadGeometry(t *testing.T) {
	if _, err := CreateWith(&memBacking{}, 64*1024, 513); err == nil {
		t.Fatal("non-power-of-two block size: want error")
	}
	if _, err := CreateWith(&memBacking{}, 100, 512); err == nil {
		t.Fatal("capacity too small: want error")
	}
}

func TestCreateWithTruncateError(t *testing.T) {
	if _, err := CreateWith(&memBacking{failTruncate: true}, 64*1024, 512); !errors.Is(err, errInjected) {
		t.Fatalf("want injected truncate error, got %v", err)
	}
}

func TestCreateWithSyncError(t *testing.T) {
	// Truncate succeeds; the first WriteAt inside sync fails.
	if _, err := CreateWith(&memBacking{failWriteAt: true}, 64*1024, 512); !errors.Is(err, errInjected) {
		t.Fatalf("want injected sync error, got %v", err)
	}
}

// validImage returns the bytes of a freshly created, closed pool with one
// volume — a valid on-backing image to mutate for the OpenWith error cases.
func validImage(t *testing.T) []byte {
	t.Helper()
	mb := &memBacking{}
	p, err := CreateWith(mb, 64*1024, 512)
	if err != nil {
		t.Fatalf("CreateWith: %v", err)
	}
	if _, err := p.CreateVolume("v", 4096); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out := make([]byte, len(mb.data))
	copy(out, mb.data)
	return out
}

func TestOpenWithReadHeaderError(t *testing.T) {
	if _, err := OpenWith(&memBacking{failReadAt: true}); err == nil {
		t.Fatal("read-header failure: want error")
	}
}

func TestOpenWithBadMagic(t *testing.T) {
	img := validImage(t)
	for i := 0; i < 8; i++ {
		img[i] = 0
	}
	if _, err := OpenWith(&memBacking{data: img}); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("want ErrBadMagic, got %v", err)
	}
}

func TestOpenWithBadVersion(t *testing.T) {
	img := validImage(t)
	binary.BigEndian.PutUint32(img[8:], version+1)
	if _, err := OpenWith(&memBacking{data: img}); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("want ErrUnsupportedVersion, got %v", err)
	}
}

func TestOpenWithReadMetadataError(t *testing.T) {
	img := validImage(t)
	// Point the metadata past the end of the image so its read short-reads.
	binary.BigEndian.PutUint64(img[32:], uint64(len(img))*4)
	if _, err := OpenWith(&memBacking{data: img}); err == nil {
		t.Fatal("read-metadata failure: want error")
	}
}

func TestOpenWithCorruptMetadata(t *testing.T) {
	img := validImage(t)
	// Re-point metadata into the (zeroed) data region: not valid gob.
	binary.BigEndian.PutUint64(img[24:], uint64(512)) // metaOffset = block 0 of data
	binary.BigEndian.PutUint64(img[32:], uint64(16))  // small metaLen of zeros
	if _, err := OpenWith(&memBacking{data: img}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

func TestOpenWithRefcountMismatch(t *testing.T) {
	img := validImage(t)
	// Claim a different physical-block count than the persisted refcount slice.
	orig := binary.BigEndian.Uint32(img[16:])
	binary.BigEndian.PutUint32(img[16:], orig+1)
	if _, err := OpenWith(&memBacking{data: img}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt (refcount mismatch), got %v", err)
	}
}
