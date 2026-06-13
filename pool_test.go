// Copyright (c) 2026, go-volumes
// SPDX-License-Identifier: BSD-3-Clause

package pool

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

func block(b byte) []byte {
	p := make([]byte, 4096)
	for i := range p {
		p[i] = b
	}
	return p
}

// TestCoWSnapshotIsolation: a snapshot keeps the data it had at snapshot time
// even after the live volume overwrites those blocks (copy-on-write).
func TestCoWSnapshotIsolation(t *testing.T) {
	p, err := Create(filepath.Join(t.TempDir(), "p.pool"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	v, err := p.CreateVolume("root", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.WriteAt(block('A'), 0); err != nil {
		t.Fatal(err)
	}
	snap, err := p.Snapshot("root", "snap0")
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the live block.
	if _, err := v.WriteAt(block('B'), 0); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4096)
	if _, err := v.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, block('B')) {
		t.Errorf("live volume: not B")
	}
	sgot := make([]byte, 4096)
	if _, err := snap.ReadAt(sgot, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sgot, block('A')) {
		t.Errorf("snapshot was disturbed: not A")
	}
}

// TestPoolFullGraceful is the anti-LVM thesis: when the pool fills, a CoW write
// fails cleanly with ErrPoolFull, the live map is unchanged, and the snapshot
// remains fully readable and correct — no corruption.
func TestPoolFullGraceful(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.pool")
	// Tiny pool: header block + a handful of data blocks.
	p, err := Create(path, 6*4096)
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.CreateVolume("root", 64*4096) // logical bigger than physical
	if err != nil {
		t.Fatal(err)
	}
	// Fill a couple of distinct blocks, then snapshot.
	if _, err := v.WriteAt(block('A'), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := v.WriteAt(block('A'), 4096); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Snapshot("root", "snap0"); err != nil {
		t.Fatal(err)
	}
	// Now overwrite + write new blocks until the pool is exhausted. After the
	// snapshot, every overwrite needs a fresh (CoW) block.
	var hitFull bool
	for i := int64(0); i < 64; i++ {
		if _, err := v.WriteAt(block('B'), i*4096); err != nil {
			if errors.Is(err, ErrPoolFull) {
				hitFull = true
				break
			}
			t.Fatalf("unexpected write error: %v", err)
		}
	}
	if !hitFull {
		t.Fatal("expected ErrPoolFull but pool never filled")
	}
	// The snapshot must still read back the original 'A' blocks intact.
	snap, err := p.OpenVolume("snap0")
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, 4096} {
		got := make([]byte, 4096)
		if _, err := snap.ReadAt(got, off); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, block('A')) {
			t.Fatalf("snapshot block at %d corrupted after pool-full", off)
		}
	}
	// Pool must reopen cleanly (metadata not corrupted).
	p.Close()
	p2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after pool-full: %v", err)
	}
	defer p2.Close()
	snap2, _ := p2.OpenVolume("snap0")
	got := make([]byte, 4096)
	snap2.ReadAt(got, 0)
	if !bytes.Equal(got, block('A')) {
		t.Fatal("snapshot corrupted across reopen")
	}
}

// TestPersistAcrossReopen: volumes, data and CoW state survive close/open.
func TestPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.pool")
	p, err := Create(path, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := p.CreateVolume("root", 1<<20)
	v.WriteAt(block('X'), 8192)
	p.Snapshot("root", "s1")
	v.WriteAt(block('Y'), 8192)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	v2, _ := p2.OpenVolume("root")
	s2, _ := p2.OpenVolume("s1")
	gv := make([]byte, 4096)
	v2.ReadAt(gv, 8192)
	gs := make([]byte, 4096)
	s2.ReadAt(gs, 8192)
	if !bytes.Equal(gv, block('Y')) {
		t.Error("live volume lost its data across reopen")
	}
	if !bytes.Equal(gs, block('X')) {
		t.Error("snapshot lost its data across reopen")
	}
}

// TestUnalignedReadWrite exercises sub-block (read-modify-write) access, which
// a filesystem driver does (e.g. a 512-byte superblock inside a 4 KiB block).
func TestUnalignedReadWrite(t *testing.T) {
	p, _ := Create(filepath.Join(t.TempDir(), "p.pool"), 4<<20)
	defer p.Close()
	v, _ := p.CreateVolume("root", 1<<20)
	want := []byte("hello unaligned world")
	if _, err := v.WriteAt(want, 4090); err != nil { // spans the 4096 boundary
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := v.ReadAt(got, 4090); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("unaligned RW: got %q want %q", got, want)
	}
}
