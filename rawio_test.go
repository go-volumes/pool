// Copyright (c) 2026, go-volumes
// SPDX-License-Identifier: BSD-3-Clause

package pool

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestCloneDiverges: a writable clone shares its origin's data but diverges
// independently — writing the clone leaves the origin (and a snapshot) intact.
func TestCloneDiverges(t *testing.T) {
	p, _ := Create(filepath.Join(t.TempDir(), "p.pool"), 8<<20)
	defer p.Close()
	v, _ := p.CreateVolume("golden", 1<<20)
	v.WriteAt(block('G'), 0)
	p.Snapshot("golden", "snap")

	clone, err := p.Clone("snap", "vm1")
	if err != nil {
		t.Fatal(err)
	}
	if clone.ReadOnly() {
		t.Fatal("clone should be writable")
	}
	// Clone starts as a copy of the golden data.
	got := make([]byte, 4096)
	clone.ReadAt(got, 0)
	if !bytes.Equal(got, block('G')) {
		t.Fatal("clone did not inherit origin data")
	}
	// Diverge the clone.
	if _, err := clone.WriteAt(block('C'), 0); err != nil {
		t.Fatal(err)
	}
	// Origin volume + snapshot are untouched.
	gv, _ := p.OpenVolume("golden")
	og := make([]byte, 4096)
	gv.ReadAt(og, 0)
	if !bytes.Equal(og, block('G')) {
		t.Error("origin disturbed by clone write")
	}
	sn, _ := p.OpenVolume("snap")
	os := make([]byte, 4096)
	sn.ReadAt(os, 0)
	if !bytes.Equal(os, block('G')) {
		t.Error("snapshot disturbed by clone write")
	}
}

// TestRawExportImportRoundTrip: export a volume (with holes) to a sparse raw
// file and re-import it — content matches, and all-zero regions stay holes.
func TestRawExportImportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p, _ := Create(filepath.Join(dir, "p.pool"), 16<<20)
	v, _ := p.CreateVolume("disk", 1<<20)
	v.WriteAt(block('A'), 0)
	v.WriteAt(block('B'), 64*4096) // leave a big hole in between
	raw := filepath.Join(dir, "disk.raw")
	if err := v.ExportRawFile(raw); err != nil {
		t.Fatal(err)
	}
	p.Close()

	// The exported file should be the logical size but sparse (allocated size
	// far smaller than 1 MiB).
	fi, _ := os.Stat(raw)
	if fi.Size() != 1<<20 {
		t.Errorf("raw size = %d, want %d", fi.Size(), 1<<20)
	}

	p2, _ := Create(filepath.Join(dir, "p2.pool"), 16<<20)
	defer p2.Close()
	v2, err := p2.ImportRawFile("disk", raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		off  int64
		want []byte
	}{
		{0, block('A')},
		{64 * 4096, block('B')},
		{8 * 4096, make([]byte, 4096)}, // a hole → zeros
	} {
		got := make([]byte, 4096)
		v2.ReadAt(got, tc.off)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("import @%d mismatch", tc.off)
		}
	}
	// The middle hole must have imported thinly (stayed a hole, not allocated).
	free, total := p2.Capacity()
	if used := total - free; used > 16 { // only ~2 data blocks + a little
		t.Errorf("import was not thin: %d/%d blocks used", used, total)
	}
}
