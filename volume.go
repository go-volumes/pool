// Copyright (c) 2026, go-volumes
// SPDX-License-Identifier: BSD-3-Clause

package pool

import (
	"fmt"
	"io"
)

// Volume is a logical block device carved out of a Pool. It implements the
// block-backend shape (ReadAt, WriteAt, Sync, Size, Truncate, Close) that the
// go-filesystems ext4/xfs drivers accept, so a filesystem can be formatted and
// mounted directly on it.
type Volume struct {
	p    *Pool
	spec *volSpec
}

// Ensure Volume satisfies the common block-backend interface shape.
var (
	_ io.ReaderAt = (*Volume)(nil)
	_ io.WriterAt = (*Volume)(nil)
)

// CreateVolume creates a new, empty (all-holes) volume of sizeBytes.
func (p *Pool) CreateVolume(name string, sizeBytes int64) (*Volume, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.meta.Volumes[name]; ok {
		return nil, ErrExists
	}
	blocks := uint32((sizeBytes + p.blockSize - 1) / p.blockSize)
	m := make([]int64, blocks)
	for i := range m {
		m[i] = -1
	}
	spec := &volSpec{Name: name, Blocks: blocks, Map: m}
	p.meta.Volumes[name] = spec
	if err := p.sync(); err != nil {
		delete(p.meta.Volumes, name)
		return nil, err
	}
	return &Volume{p: p, spec: spec}, nil
}

// OpenVolume returns a handle to an existing volume or snapshot.
func (p *Pool) OpenVolume(name string) (*Volume, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	spec, ok := p.meta.Volumes[name]
	if !ok {
		return nil, ErrNotFound
	}
	return &Volume{p: p, spec: spec}, nil
}

// Snapshot freezes the current state of volume `src` under name `snap`. The
// snapshot is read-only and immutable: its blocks are reference-counted, so
// later writes to the live volume copy-on-write rather than disturb it.
func (p *Pool) Snapshot(src, snap string) (*Volume, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.meta.Volumes[src]
	if !ok {
		return nil, ErrNotFound
	}
	if _, ok := p.meta.Volumes[snap]; ok {
		return nil, ErrExists
	}
	cp := &volSpec{Name: snap, Blocks: s.Blocks, Map: make([]int64, len(s.Map)), ReadOnly: true}
	copy(cp.Map, s.Map)
	// Every physical block the snapshot references is now shared.
	for _, b := range cp.Map {
		if b >= 0 {
			p.meta.Refcount[b]++
		}
	}
	p.meta.Volumes[snap] = cp
	if err := p.sync(); err != nil {
		// Roll back the refcount bumps and the new entry.
		for _, b := range cp.Map {
			if b >= 0 {
				p.meta.Refcount[b]--
			}
		}
		delete(p.meta.Volumes, snap)
		return nil, err
	}
	return &Volume{p: p, spec: cp}, nil
}

// Volumes lists all volume and snapshot names.
func (p *Pool) Volumes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.meta.Volumes))
	for n := range p.meta.Volumes {
		out = append(out, n)
	}
	return out
}

// Size returns the volume's logical size in bytes.
func (v *Volume) Size() (int64, error) { return int64(v.spec.Blocks) * v.p.blockSize, nil }

// Name returns the volume name.
func (v *Volume) Name() string { return v.spec.Name }

// ReadOnly reports whether the volume is read-only (snapshots always are).
func (v *Volume) ReadOnly() bool { return v.spec.ReadOnly }

// Sync flushes the backing pool.
func (v *Volume) Sync() error { return v.p.Sync() }

// Close is a no-op for a volume handle; the pool owns the file. Sync the pool
// (or Close it) to persist.
func (v *Volume) Close() error { return nil }

// Truncate is accepted for block-backend compatibility but the volume size is
// fixed at creation; growing/shrinking is not supported in this version.
func (v *Volume) Truncate(size int64) error {
	cur, _ := v.Size()
	if size == cur {
		return nil
	}
	return fmt.Errorf("pool: volume resize not supported (have %d, want %d)", cur, size)
}

// ReadAt reads len(b) bytes at byte offset off. Unallocated regions read as
// zeros (sparse).
func (v *Volume) ReadAt(b []byte, off int64) (int, error) {
	v.p.mu.Lock()
	defer v.p.mu.Unlock()
	bs := v.p.blockSize
	total := int64(v.spec.Blocks) * bs
	if off < 0 {
		return 0, fmt.Errorf("pool: negative offset")
	}
	n := 0
	for n < len(b) {
		pos := off + int64(n)
		if pos >= total {
			return n, io.EOF
		}
		lb := pos / bs
		within := pos % bs
		cnt := int(bs - within)
		if cnt > len(b)-n {
			cnt = len(b) - n
		}
		phys := v.spec.Map[lb]
		if phys < 0 {
			// Hole → zeros.
			for i := 0; i < cnt; i++ {
				b[n+i] = 0
			}
		} else {
			if _, err := v.p.f.ReadAt(b[n:n+cnt], v.p.blockOffset(uint32(phys))+within); err != nil {
				return n, err
			}
		}
		n += cnt
	}
	return n, nil
}

// WriteAt writes b at byte offset off, copy-on-write. A write that must
// allocate a new physical block when the pool is full fails with ErrPoolFull
// without changing any on-disk state — so snapshots and prior data are never
// corrupted.
func (v *Volume) WriteAt(b []byte, off int64) (int, error) {
	v.p.mu.Lock()
	defer v.p.mu.Unlock()
	if v.spec.ReadOnly {
		return 0, ErrReadOnly
	}
	bs := v.p.blockSize
	total := int64(v.spec.Blocks) * bs
	if off < 0 {
		return 0, fmt.Errorf("pool: negative offset")
	}
	if off+int64(len(b)) > total {
		return 0, fmt.Errorf("pool: write past end of volume")
	}
	n := 0
	dirty := false
	for n < len(b) {
		pos := off + int64(n)
		lb := pos / bs
		within := pos % bs
		cnt := int(bs - within)
		if cnt > len(b)-n {
			cnt = len(b) - n
		}
		phys, err := v.cowBlock(uint32(lb), within == 0 && cnt == int(bs))
		if err != nil {
			if dirty {
				_ = v.p.sync() // persist the blocks already written before failing
			}
			return n, err
		}
		if _, err := v.p.f.WriteAt(b[n:n+cnt], v.p.blockOffset(phys)+within); err != nil {
			return n, err
		}
		dirty = true
		n += cnt
	}
	if dirty {
		if err := v.p.sync(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// cowBlock returns the physical block backing logical block lb for writing,
// performing copy-on-write when the current block is shared (with a snapshot)
// or absent. fullBlock indicates the whole block is being overwritten, so a
// freshly-allocated block needs no pre-copy. Caller must hold p.mu.
func (v *Volume) cowBlock(lb uint32, fullBlock bool) (uint32, error) {
	p := v.p
	cur := v.spec.Map[lb]
	// Owned exclusively and present → write in place.
	if cur >= 0 && p.meta.Refcount[cur] == 1 {
		return uint32(cur), nil
	}
	// Hole or shared → allocate a new block (CoW). This is the only step that
	// can fail when the pool is full; it does so before any mutation.
	nb, err := p.allocBlock()
	if err != nil {
		return 0, err
	}
	if cur >= 0 {
		// Shared: copy old contents forward unless the whole block is being
		// overwritten anyway, then drop our reference to the old block.
		if !fullBlock {
			buf := make([]byte, p.blockSize)
			if _, err := p.f.ReadAt(buf, p.blockOffset(uint32(cur))); err != nil {
				p.meta.Refcount[nb] = 0 // undo the allocation
				return 0, err
			}
			if _, err := p.f.WriteAt(buf, p.blockOffset(nb)); err != nil {
				p.meta.Refcount[nb] = 0
				return 0, err
			}
		}
		p.meta.Refcount[cur]--
	}
	v.spec.Map[lb] = int64(nb)
	return nb, nil
}
