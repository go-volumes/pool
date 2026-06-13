// Copyright (c) 2026, go-volumes
// SPDX-License-Identifier: BSD-3-Clause

package pool

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

// ExportRaw writes the volume's logical contents to w as a flat ("raw") image.
// Only allocated blocks are written; unallocated (hole) blocks are skipped, so
// when w is a freshly-truncated file the result is a sparse raw image. This is
// the bridge to consumers that only accept raw files — e.g. Apple's
// Virtualization.framework (vz), which attaches raw disk images at the kernel
// level and cannot call a Go block backend. Clone a golden volume, ExportRaw
// the clone, hand the raw file to vz; ImportRaw the modified file back to
// capture changes.
func (v *Volume) ExportRaw(w io.WriterAt) error {
	v.p.mu.Lock()
	defer v.p.mu.Unlock()
	bs := v.p.blockSize
	for lb := uint32(0); lb < v.spec.Blocks; lb++ {
		phys := v.spec.Map[lb]
		if phys < 0 {
			continue // hole → leave sparse
		}
		buf := make([]byte, bs)
		if _, err := v.p.f.ReadAt(buf, v.p.blockOffset(uint32(phys))); err != nil {
			return fmt.Errorf("pool: export read block %d: %w", lb, err)
		}
		if _, err := w.WriteAt(buf, int64(lb)*bs); err != nil {
			return fmt.Errorf("pool: export write block %d: %w", lb, err)
		}
	}
	return nil
}

// ExportRawFile writes the volume to a sparse raw file at path (overwriting it),
// truncated to the volume's logical size. The file is suitable to attach to a
// VM that requires raw images.
func (v *Volume) ExportRawFile(path string) error {
	size, _ := v.Size()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("pool: export %s: %w", path, err)
	}
	if err := f.Truncate(size); err != nil { // sparse backing
		f.Close()
		return err
	}
	if err := v.ExportRaw(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ImportRaw creates a new volume named `name` of the given size and populates
// it from the raw image r. All-zero blocks are left unallocated (holes), so a
// sparse or zero-padded raw image imports thinly. Useful to bring a raw image
// (e.g. one a vz guest wrote to) back under pool management — snapshot it
// afterwards to capture a point in time.
func (p *Pool) ImportRaw(name string, r io.ReaderAt, size int64) (*Volume, error) {
	v, err := p.CreateVolume(name, size)
	if err != nil {
		return nil, err
	}
	bs := v.p.blockSize
	zero := make([]byte, bs)
	buf := make([]byte, bs)
	for off := int64(0); off < size; off += bs {
		n := bs
		if size-off < n {
			n = size - off
		}
		rd := buf[:n]
		if _, err := r.ReadAt(rd, off); err != nil && err != io.EOF {
			return nil, fmt.Errorf("pool: import read @%d: %w", off, err)
		}
		if bytes.Equal(rd, zero[:n]) {
			continue // all-zero → keep it a hole (thin)
		}
		if _, err := v.WriteAt(rd, off); err != nil {
			return nil, fmt.Errorf("pool: import write @%d: %w", off, err)
		}
	}
	return v, nil
}

// ImportRawFile is ImportRaw reading from a file at path.
func (p *Pool) ImportRawFile(name, path string) (*Volume, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pool: import %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return p.ImportRaw(name, f, fi.Size())
}
