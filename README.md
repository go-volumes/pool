# pool

A pure-Go, copy-on-write **pooled volume manager** — a small, ZFS-inspired
alternative to LVM thin provisioning. No cgo, no root, no device-mapper.

A pool owns a flat array of fixed-size physical blocks backed by a single file.
**Volumes** are logical block devices carved out of the pool; **snapshots** are
immutable, reference-counted captures of a volume. Writes are copy-on-write, so
overwriting a block shared with a snapshot allocates a fresh block and leaves
the snapshot untouched.

## Why not LVM?

LVM thin snapshots can **corrupt** when the pool fills past a threshold: ext4 (or
XFS) overwrites in place and assumes its blocks are durable, but the thin pool
needs a *new* block for the copy-on-write — and if none is free, the write fails
mid-operation and the filesystem/snapshot is left inconsistent.

`pool` is built so that can't happen:

- A **snapshot is immutable** — its blocks are reference-counted and never
  rewritten, so it can't be corrupted by the live volume filling up.
- When the pool is full, a copy-on-write write fails **cleanly and atomically**
  with `ErrPoolFull`, *before* any on-disk state changes. A filesystem mounted
  on the volume sees a normal `ENOSPC`/`EIO` (which ext4/XFS handle) rather than
  silent corruption.

This is the ZFS behaviour (refuse the write, keep snapshots intact), brought to
non-CoW filesystems via a block layer underneath them.

## Filesystem-agnostic

A `Volume` implements `ReadAt`, `WriteAt`, `Sync`, `Size`, `Truncate`, `Close`
— the same block-backend shape the
[`go-filesystems`](https://github.com/go-filesystems) ext4/xfs drivers accept
(`OpenFromDevice`). So you can format and mount **ext4, XFS, or any block-based
driver** straight onto a pool volume, and it composes with
[`go-fde`](https://github.com/go-fde) for encryption (pool → fde → fs).

Note: XFS historically had no free integrated volume manager (SGI's XLV/XVM were
proprietary), so a CoW pool under XFS fills a real gap.

**Verified end-to-end:** a real ext4 filesystem (the `go-filesystems/ext4`
driver) was run live on a pool volume via `OpenFromDevice`, a file was written,
the volume snapshotted, then the live file overwritten and a new file added.
Read back through ext4, the **live** volume showed the new state while the
**snapshot** showed the exact pre-snapshot filesystem — ext4 never knew it was
on a CoW pool, and the snapshot was fully isolated.

## Usage

```go
p, _ := pool.Create("data.pool", 64<<20) // 64 MiB pool
defer p.Close()

vol, _ := p.CreateVolume("root", 32<<20)
vol.WriteAt(data, off)                   // copy-on-write

p.Snapshot("root", "before-upgrade")     // immutable snapshot
// ... keep writing to vol; the snapshot is undisturbed ...

snap, _ := p.OpenVolume("before-upgrade")
snap.ReadAt(buf, off)                    // original bytes

// Instant, space-shared writable clone (ZFS-style).
clone, _ := p.Clone("before-upgrade", "vm1")
clone.WriteAt(data, off)                 // diverges from the origin, CoW
```

## Raw images (e.g. Apple Virtualization.framework / vz)

Some consumers only accept **raw disk files** and do block I/O on them at the
kernel level — they can't call a Go block backend. Apple's
Virtualization.framework (vz) is the prime example. The pool bridges to them:
keep golden images + thin CoW clones + snapshots in the pool, then **export a
clone to a sparse raw file** for the consumer, and **import the modified file
back** to capture changes.

```go
clone, _ := p.Clone("golden", "vm1")
clone.ExportRawFile("vm1.raw")           // sparse raw image for vz to boot
// ... vz runs and writes vm1.raw ...
p.ImportRawFile("vm1-after", "vm1.raw")  // back under pool management; snapshot it
```

This gives raw-only hypervisors what they lack natively — instant golden-image
clones, thin provisioning, and safe snapshots — while they still see just a raw
file.

## Status / limitations

- Single backing file (multi-device pooling / RAID is a planned follow-up).
- Fixed volume size at creation; online grow/shrink not yet implemented.
- Linear free-block search (fine for moderate pools; a free bitmap is a follow-up).
