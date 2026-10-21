#!/usr/bin/env bash
# Workload for etcd #22498 (https://github.com/etcd-io/etcd/issues/22498).
# unsynced runs it with the mount as its working directory:
#
#	unsynced record -init examples/etcd22498/seed -out OUT examples/etcd22498/workload.sh
#
# m3's data dir is ./m3, on the mount (from seed/). m1 and m2 run off the
# mount, from copies of peers/ in $PEERS (default /tmp/unsynced/etcd22498/peers),
# which check.sh uses afterwards.
#
#  1. With m3 down, write 300 keys through m1 and m2, and ACK each one once
#     etcd has confirmed it.
#  2. Start m3. It is too far behind, so the leader sends it a snapshot:
#     m3 renames <index>.snap.db over member/snap/db, then its old db's final
#     commit writes the snapshot's index into the old file.
#  3. Stop everything once m3 logs "closed old backend file".
#
# The bug shows in the crash state right after that final commit (the old db
# inode's last FDATASYNC after the rename), with the rename not persisted:
# etcd never fsyncs member/snap after it.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=common.sh
source "$HERE/common.sh"
PEERS=${PEERS:-/tmp/unsynced/etcd22498/peers}
trap stop_all EXIT

rm -rf "$PEERS"
mkdir -p "$PEERS"
cp -a "$HERE/peers/m1" "$HERE/peers/m2" "$PEERS/"
start_member 1 "$PEERS/m1" "$PEERS/m1.log"
start_member 2 "$PEERS/m2" "$PEERS/m2.log"
wait_healthy 1 30
wait_healthy 2 30

for n in $(seq -w 0 299); do
	ctl --endpoints="$(ep 1),$(ep 2)" put "k-$n" "v-$n" >/dev/null
	echo "k-$n" >>.ack
done

start_member 3 ./m3 "$PEERS/m3.log"
for _ in $(seq 240); do
	grep -q '"msg":"closed old backend file"' "$PEERS/m3.log" 2>/dev/null && break
	sleep 0.25
done
grep -q '"msg":"closed old backend file"' "$PEERS/m3.log" || {
	echo "m3 did not install a snapshot, see $PEERS/m3.log" >&2
	exit 1
}
sleep 1
