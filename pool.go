// Copyright (c) 2026, go-volumes
// SPDX-License-Identifier: BSD-3-Clause

package pool

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"os"
	"sync"
)

const (
	magic        = 0x474F504C32303236 // "GOPL2026"
	version      = 1
	headerSize   = 64
	defaultBlock = 4096
)

// Pool is an open copy-on-write block pool backed by a single file.
//
// On-disk layout:
//
//	[0, blockSize)                 header
//	[blockSize, blockSize+N*BS)    data region: N physical blocks
//	[metaOffset, metaOffset+metaLen) gob-encoded metadata (refcounts + volumes)
//
// The data region is fixed; metadata is rewritten after the data region on
// every Sync (its offset/length live in the header).
type Pool struct {
	mu        sync.Mutex
	f         Backing
	blockSize int64
	dataStart int64  // byte offset of physical block 0
	nblocks   uint32 // number of physical data blocks (capacity)

	meta metadata
}

// metadata is the gob-serialised pool state.
type metadata struct {
	Refcount []uint32            // one entry per physical block; 0 = free
	Volumes  map[string]*volSpec // live volumes and snapshots
}

// volSpec is the persisted state of one volume or snapshot.
type volSpec struct {
	Name     string
	Blocks   uint32  // logical size in blocks
	Map      []int64 // logical block → physical block; -1 = unallocated hole
	ReadOnly bool    // snapshots are read-only
}

// Create makes a new pool at path with room for capacityBytes of data,
// rounded down to whole blocks. An existing file is overwritten.
func Create(path string, capacityBytes int64) (*Pool, error) {
	return CreateBlock(path, capacityBytes, defaultBlock)
}

// CreateBlock is Create with an explicit block size (power of two ≥ 512).
func CreateBlock(path string, capacityBytes, blockSize int64) (*Pool, error) {
	// Validate before touching the filesystem, so bad geometry never leaves a
	// stray file behind.
	if _, err := validateGeom(capacityBytes, blockSize); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("pool: create %s: %w", path, err)
	}
	p, err := CreateWith(f, capacityBytes, blockSize)
	if err != nil {
		f.Close()
		return nil, err
	}
	return p, nil
}

// CreateWith makes a new pool on an arbitrary [Backing] — a memory buffer, an
// S3-backed chunk store, an NBD export — rather than a local file. The backing
// is assumed empty; CreateWith reserves the data region on it. On success the
// pool owns the backing and [Pool.Close] closes it; on error the caller keeps
// ownership of b.
func CreateWith(b Backing, capacityBytes, blockSize int64) (*Pool, error) {
	n, err := validateGeom(capacityBytes, blockSize)
	if err != nil {
		return nil, err
	}
	p := &Pool{
		f:         b,
		blockSize: blockSize,
		dataStart: blockSize,
		nblocks:   uint32(n),
		meta: metadata{
			Refcount: make([]uint32, n),
			Volumes:  map[string]*volSpec{},
		},
	}
	// Reserve the data region.
	if err := b.Truncate(p.dataStart + int64(n)*blockSize); err != nil {
		return nil, fmt.Errorf("pool: truncate: %w", err)
	}
	if err := p.sync(); err != nil {
		return nil, err
	}
	return p, nil
}

// validateGeom checks the pool geometry and returns the data-block count.
func validateGeom(capacityBytes, blockSize int64) (int64, error) {
	if blockSize < 512 || blockSize&(blockSize-1) != 0 {
		return 0, fmt.Errorf("pool: block size %d must be a power of two ≥ 512", blockSize)
	}
	n := capacityBytes / blockSize
	if n <= 0 {
		return 0, fmt.Errorf("pool: capacity %d too small for block size %d", capacityBytes, blockSize)
	}
	return n, nil
}

// Open opens an existing file-backed pool.
func Open(path string) (*Pool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("pool: open %s: %w", path, err)
	}
	p, err := OpenWith(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return p, nil
}

// OpenWith opens an existing pool that lives on an arbitrary [Backing]. On
// success the pool owns the backing and [Pool.Close] closes it; on error the
// caller keeps ownership of b.
func OpenWith(b Backing) (*Pool, error) {
	hdr := make([]byte, headerSize)
	if _, err := b.ReadAt(hdr, 0); err != nil {
		return nil, fmt.Errorf("pool: read header: %w", err)
	}
	be := binary.BigEndian
	if be.Uint64(hdr[0:]) != magic {
		return nil, ErrBadMagic
	}
	if be.Uint32(hdr[8:]) != version {
		return nil, ErrUnsupportedVersion
	}
	p := &Pool{
		f:         b,
		blockSize: int64(be.Uint32(hdr[12:])),
		nblocks:   be.Uint32(hdr[16:]),
	}
	p.dataStart = p.blockSize
	metaOffset := int64(be.Uint64(hdr[24:]))
	metaLen := int64(be.Uint64(hdr[32:]))
	blob := make([]byte, metaLen)
	if _, err := b.ReadAt(blob, metaOffset); err != nil {
		return nil, fmt.Errorf("pool: read metadata: %w", err)
	}
	if err := gobDecode(blob, &p.meta); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if uint32(len(p.meta.Refcount)) != p.nblocks {
		return nil, fmt.Errorf("%w: refcount length %d != nblocks %d", ErrCorrupt, len(p.meta.Refcount), p.nblocks)
	}
	return p, nil
}

// Sync flushes metadata and data to disk.
func (p *Pool) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sync()
}

func (p *Pool) sync() error {
	blob, err := gobEncode(&p.meta)
	if err != nil {
		return err
	}
	metaOffset := p.dataStart + int64(p.nblocks)*p.blockSize
	if _, err := p.f.WriteAt(blob, metaOffset); err != nil {
		return fmt.Errorf("pool: write metadata: %w", err)
	}
	if err := p.f.Truncate(metaOffset + int64(len(blob))); err != nil {
		return fmt.Errorf("pool: truncate metadata: %w", err)
	}
	hdr := make([]byte, headerSize)
	be := binary.BigEndian
	be.PutUint64(hdr[0:], magic)
	be.PutUint32(hdr[8:], version)
	be.PutUint32(hdr[12:], uint32(p.blockSize))
	be.PutUint32(hdr[16:], p.nblocks)
	be.PutUint64(hdr[24:], uint64(metaOffset))
	be.PutUint64(hdr[32:], uint64(len(blob)))
	if _, err := p.f.WriteAt(hdr, 0); err != nil {
		return fmt.Errorf("pool: write header: %w", err)
	}
	return p.f.Sync()
}

// Close syncs and closes the pool.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f == nil {
		return nil
	}
	err := p.sync()
	cerr := p.f.Close()
	p.f = nil
	if err != nil {
		return err
	}
	return cerr
}

// Capacity returns (freeBlocks, totalBlocks).
func (p *Pool) Capacity() (free, total uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, rc := range p.meta.Refcount {
		if rc == 0 {
			free++
		}
	}
	return free, p.nblocks
}

// allocBlock reserves one free physical block (refcount→1) and returns its
// index, or ErrPoolFull if none are free. Caller must hold p.mu.
func (p *Pool) allocBlock() (uint32, error) {
	for i, rc := range p.meta.Refcount {
		if rc == 0 {
			p.meta.Refcount[i] = 1
			return uint32(i), nil
		}
	}
	return 0, ErrPoolFull
}

// blockOffset returns the byte offset of physical block b.
func (p *Pool) blockOffset(b uint32) int64 { return p.dataStart + int64(b)*p.blockSize }

func gobEncode(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func gobDecode(data []byte, v any) error {
	return gob.NewDecoder(bytes.NewReader(data)).Decode(v)
}
