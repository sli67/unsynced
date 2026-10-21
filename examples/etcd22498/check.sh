#!/usr/bin/env bash
# Checker for etcd #22498: start m3 from a crash state, next to m1 and m2 as
# workload.sh left them, and check that m3 has every acked key.
#
#	check.sh STATE_DIR ACK_FILE
#
# STATE_DIR is a crash state from `unsynced simulate -out STATE_DIR` (it holds
# m3/), and ACK_FILE is STATE_DIR.ack, which simulate writes next to it: the
# acks made before the crash, one key per line. Neither is
# changed: m1, m2 and m3 run on copies, in a new folder under
# /tmp/unsynced/etcd22498. Exit status 0 = OK, 1 = BUG, 2 = usage/setup error.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=common.sh
source "$HERE/common.sh"
STATE=${1:?usage: check.sh STATE_DIR ACK_FILE}
ACKS=${2:?usage: check.sh STATE_DIR ACK_FILE}
PEERS=${PEERS:-/tmp/unsynced/etcd22498/peers}
[[ -d "$STATE/m3" && -d "$PEERS/m1" && -d "$PEERS/m2" ]] || {
	echo "need $STATE/m3 and $PEERS/m1, $PEERS/m2 (run workload.sh first)" >&2
	exit 2
}
trap stop_all EXIT

mkdir -p /tmp/unsynced/etcd22498
W=$(mktemp -d /tmp/unsynced/etcd22498/check.XXXX)
cp -a "$PEERS/m1" "$PEERS/m2" "$W/"
cp -a "$STATE/m3" "$W/m3"

# m1 and m2 start fresh: a leader that kept running would remember how far m3
# got in the recorded run, past the crash point.
start_member 1 "$W/m1" "$W/m1.log"
start_member 2 "$W/m2" "$W/m2.log"
wait_healthy 1 30 && wait_healthy 2 30 || { echo "m1/m2 did not start, see $W" >&2; exit 2; }
start_member 3 "$W/m3" "$W/m3.log"
if ! wait_healthy 3 30; then
	echo "BUG: m3 did not come up from the crash state, see $W/m3.log"
	exit 1
fi
sleep 2 # let the leader send m3 anything it is missing

# Serializable reads: m3 answers from its own data, without asking the leader.
missing=$(comm -23 <(sort -u "$ACKS") \
	<(ctl --endpoints="$(ep 3)" get "" --from-key --keys-only --consistency=s | grep -v '^$' | sort -u))
total=$(sort -u "$ACKS" | wc -l)
if [[ -n "$missing" ]]; then
	echo "BUG: m3 is missing $(wc -l <<<"$missing") of $total acked keys (first: $(head -1 <<<"$missing")); logs in $W"
	exit 1
fi
echo "OK: m3 has all $total acked keys"
