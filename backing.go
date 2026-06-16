// Copyright (c) 2026, go-volumes
// SPDX-License-Identifier: BSD-3-Clause

package pool

import "io"

// Backing is the physical byte store a Pool's data and metadata live on: a
// single local file today, an S3-backed chunk store or an NBD export tomorrow.
// The copy-on-write volume engine is identical regardless of where the bytes
// physically reside — only the Backing changes.
//
// *os.File satisfies Backing directly, so the file-backed Create / Open paths
// need no adapter. To run a pool on a different store, implement Backing and
// open the pool with [CreateWith] / [OpenWith].
type Backing interface {
	io.ReaderAt // ReadAt(p []byte, off int64) (n int, err error)
	io.WriterAt // WriteAt(p []byte, off int64) (n int, err error)

	// Truncate sets the store's size in bytes (growing reserves space, the way
	// Create reserves the data region; shrinking discards the tail).
	Truncate(size int64) error

	// Sync durably commits buffered writes.
	Sync() error

	io.Closer // Close releases the store.
}
