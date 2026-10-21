# unsynced

unsynced finds crash-consistency bugs in storage programs: data a program
said was safe that a power loss can still destroy.

It runs your program on a FUSE filesystem that records every file operation.
Then it replays that trace in a model of the disk that knows what was only in
memory and what was synced, and writes out the files a power loss would leave
at any point you choose. A checker opens those files the way the program
would recover them and checks that every promise still holds.

## How it works

1. **record** mounts a filesystem at `/tmp/unsynced/mnt`, backed by real files
   in `/tmp/unsynced/backing`, and runs your program inside it. Every operation
   goes through to the real files and is logged. Writes to a file named `.ack`
   are not stored: they are the program's promises ("key k-001 is committed").
2. **simulate** replays the log up to a crash point. Each file and directory
   has a volatile copy (what the program sees) and a durable copy (what
   survives a power loss):
   - `fsync`/`fdatasync` on a file makes its data durable;
   - `fsync` on a directory makes its names durable: creates, renames,
     unlinks. Nothing else does (strict POSIX).

   It writes the durable copy out as real files, plus the acks made before the
   crash.
3. **your checker** runs the program's recovery on those files and checks
   every acked item. It reports OK or BUG.

## Requirements

Linux with FUSE (`/dev/fuse`, `fusermount3`), Go 1.26+.

## Usage

```
go build -o /tmp/unsynced/unsynced .
unsynced record [-init DIR] [-out DIR] PROGRAM [ARGS...]
unsynced simulate [-trace FILE] [-spec FILE] [-out DIR]
```

Below, `unsynced` is that binary, `/tmp/unsynced/unsynced`.

**record** runs PROGRAM with the mount as its working directory and saves, in
`-out` (default `/tmp/unsynced`):

- `trace.jsonl`: every operation, one JSON object per line
- `data.bin`: the bytes written; the trace says where each write's bytes are
- `ack`: every ack, one per line

`-init DIR` copies DIR into the filesystem before PROGRAM starts, for programs
that recover an existing database. Its files are saved at the start of the
trace (`INIT_*` ops), so the trace works on its own.

**simulate** replays a trace (default `/tmp/unsynced/trace.jsonl`) and writes
the crash state to `-out` (default `/tmp/unsynced/crash`), and the acks made
before the crash to `<out>.ack` (`/tmp/unsynced/crash.ack`).

### Crash spec

`-spec` names a JSON file choosing the crash:

```json
{"crashAfter": 1722, "persistSeq": [1707]}
```

- `crashAfter`: the power fails right after this op (its `seq`).
- `persistSeq`: ops that were not synced yet but reached disk anyway. A
  `WRITE` brings its whole pages; a `CREATE`, `MKDIR`, `RENAME`, `UNLINK` or
  `RMDIR` brings its directory's name changes up to that point; a `TRUNCATE`,
  `FALLOCATE` or `O_TRUNC` open brings the new size.

Without `-spec`, the crash is after the last op and nothing unsynced survives.

### Acks

After each operation the storage system confirms, the program appends a line
to `.ack` in its working directory, for example the key it just wrote. A shell
script can do `echo "k-$n" >>.ack`. The checker gets `<out>.ack` and must find
every one of those items in the crash state.

## Trace format

```json
{"seq":9,"op":"WRITE","path":"my.db","off":16384,"dataOff":81920,"dataLen":4096,"inode":47877}
{"seq":12,"op":"ACK","path":".ack","ack":"k-000\n"}
```

Files are identified by `inode`; `path` is for people reading the trace, and
is empty for a file that was unlinked while still open. A write's bytes are in
`data.bin` at `dataOff`; all-zero data isn't stored (`"zeros": true`).

Recorded ops: `OPEN`, `CREATE`, `MKDIR`, `READ`, `WRITE`, `ACK`, `TRUNCATE`,
`FALLOCATE`, `FSYNC`, `FDATASYNC`, `FSYNCDIR`, `FLUSH`, `RELEASE`, `RENAME`,
`UNLINK`, `RMDIR`, plus `INIT_DIR`, `INIT_FILE` and `INIT_DATA` for `-init`.

## Example: etcd #22498

`examples/etcd22498` reproduces [etcd #22498](https://github.com/etcd-io/etcd/issues/22498):
a follower that installs a snapshot from the leader renames
`<index>.snap.db` over `member/snap/db` without fsyncing `member/snap`. The
old db's final commit then writes the snapshot's index into the old file. If
the rename is lost, the follower restarts on old data that claims to be up to
date, and never fetches the keys it is missing.

- `workload.sh`: m1 and m2 run off the mount; with m3 down, it writes 300 keys
  and acks each one, then starts m3 on the mount so the leader sends it a
  snapshot.
- `check.sh STATE ACKS`: restarts m1, m2 and m3 (from the crash state) and
  checks that m3 has every acked key.

It needs `etcd` and `etcdctl` (v3.7+) in `$PATH` and ports 26379-26580.
From the repo root:

```bash
unsynced record -init examples/etcd22498/seed examples/etcd22498/workload.sh
```

In our recording, the rename `<index>.snap.db` → `m3/member/snap/db` is op
**1707**, and the old db (now without a name) gets its final commit synced at
op **1722**. Crashing after 1722 shows the bug; crashing at the rename itself
doesn't, because the old db doesn't claim the new index yet. The numbers can
shift a little between recordings: look for the `RENAME` with
`"newPath":"m3/member/snap/db"` and the last `FDATASYNC` of the old db's inode
after it.

Crash there with the rename lost, and, as a control, with it persisted:

```bash
echo '{"crashAfter": 1722, "persistSeq": []}' >/tmp/unsynced/lost.json
echo '{"crashAfter": 1722, "persistSeq": [1707]}' >/tmp/unsynced/kept.json
for v in lost kept; do
  unsynced simulate -spec /tmp/unsynced/$v.json -out /tmp/unsynced/crash-$v
  examples/etcd22498/check.sh /tmp/unsynced/crash-$v /tmp/unsynced/crash-$v.ack
done
```

```
BUG: m3 is missing 300 of 300 acked keys
OK: m3 has all 300 acked keys
```
