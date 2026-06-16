// Copyright (c) 2026, go-volumes
// SPDX-License-Identifier: BSD-3-Clause

package pool

import volume "github.com/go-volumes/interface"

// Compile-time proof that a Volume is a go-volumes block device — the same
// Device contract an S3 backing, an NBD export, or a go-filesystems format
// driver speaks. It also reports its name and read-only status, and accepts
// (fixed-size) Truncate.
var (
	_ volume.Device           = (*Volume)(nil)
	_ volume.ReadOnly         = (*Volume)(nil)
	_ volume.Truncater        = (*Volume)(nil)
	_ volume.Named            = (*Volume)(nil)
	_ volume.ReadOnlyReporter = (*Volume)(nil)
)
