// Copyright (c) 2026, go-volumes
// SPDX-License-Identifier: BSD-3-Clause

// Package pool is a copy-on-write, pooled block-volume manager — a small,
// ZFS-inspired alternative to LVM thin provisioning.
//
// A pool owns a flat array of fixed-size physical blocks backed by a single
// file. Volumes are logical block devices carved out of the pool; each keeps a
// logical→physical block map. Writes are copy-on-write: overwriting a block
// that is shared with a snapshot allocates a fresh physical block, so the
// snapshot's data is never disturbed.
//
// The defining property — and the reason it is not LVM — is failure behaviour:
// a snapshot is immutable (its blocks are reference-counted and never
// rewritten), so it can never be corrupted by the live volume filling up.
// When the pool runs out of free blocks, a copy-on-write write fails *cleanly*
// with ErrPoolFull, atomically, before any on-disk state changes. The caller
// (or a filesystem mounted on the volume) sees a normal ENOSPC/EIO rather than
// silent corruption.
//
// Any block-device consumer can sit on a Volume: it implements ReadAt, WriteAt,
// Sync, Size, Truncate and Close, the same shape the go-filesystems ext4/xfs
// drivers accept as a BlockBackend.
package pool

import "errors"

var (
	// ErrPoolFull is returned by a write that needs to allocate a physical
	// block when none are free. The write makes no on-disk change, so
	// snapshots and already-written data stay intact.
	ErrPoolFull = errors.New("pool: no free blocks (pool full)")

	// ErrBadMagic is returned when opening a file that is not a pool.
	ErrBadMagic = errors.New("pool: bad magic")

	// ErrUnsupportedVersion is returned for an unknown on-disk version.
	ErrUnsupportedVersion = errors.New("pool: unsupported version")

	// ErrNotFound is returned when a named volume/snapshot does not exist.
	ErrNotFound = errors.New("pool: volume not found")

	// ErrExists is returned when creating a volume/snapshot whose name is taken.
	ErrExists = errors.New("pool: name already exists")

	// ErrReadOnly is returned by writes to a snapshot or a read-only volume.
	ErrReadOnly = errors.New("pool: volume is read-only")

	// ErrCorrupt is returned when on-disk metadata fails a sanity check.
	ErrCorrupt = errors.New("pool: corrupt metadata")
)
