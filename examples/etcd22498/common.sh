# Shared by workload.sh and check.sh: the 3-member cluster used to reproduce
# etcd #22498. Members get fixed names, ports and token, so a data dir from
# seed/ or peers/ always rejoins the same cluster.
# Uses 127.0.0.1 ports 26379-26580. Needs etcd and etcdctl (v3.7+) in $PATH.

CLUSTER="m1=http://127.0.0.1:26380,m2=http://127.0.0.1:26480,m3=http://127.0.0.1:26580"
cport() { echo $((26379 + ($1 - 1) * 100)); }
pport() { echo $((26380 + ($1 - 1) * 100)); }
ep() { echo "http://127.0.0.1:$(cport "$1")"; }
ctl() { etcdctl --dial-timeout=2s --command-timeout=5s "$@"; }

# member_flags IDX DATADIR: the command line for member IDX. A small
# snapshot count and catch-up window make the leader send a snapshot to a
# member that missed a few hundred writes.
member_flags() {
	echo --name "m$1" --data-dir "$2" \
		--listen-client-urls "$(ep "$1")" --advertise-client-urls "$(ep "$1")" \
		--listen-peer-urls "http://127.0.0.1:$(pport "$1")" \
		--initial-advertise-peer-urls "http://127.0.0.1:$(pport "$1")" \
		--initial-cluster "$CLUSTER" --initial-cluster-state new \
		--initial-cluster-token tok-22498 \
		--snapshot-count 50 --snapshot-catchup-entries 10 \
		--logger zap --log-outputs stderr
}

declare -A PID
start_member() { # IDX DATADIR LOGFILE
	# shellcheck disable=SC2046
	etcd $(member_flags "$1" "$2") >"$3" 2>&1 &
	PID[$1]=$!
}
stop_member() { # never fails: etcd's exit status after SIGTERM isn't 0
	kill -TERM "${PID[$1]}" 2>/dev/null
	wait "${PID[$1]}" 2>/dev/null || true
}
stop_all() { for i in "${!PID[@]}"; do stop_member "$i"; done; }

wait_healthy() { # IDX TIMEOUT_S; returns 2 if the process died
	local n=0
	while ((n < $2 * 4)); do
		kill -0 "${PID[$1]}" 2>/dev/null || return 2
		ctl --endpoints="$(ep "$1")" endpoint health >/dev/null 2>&1 && return 0
		sleep 0.25
		n=$((n + 1))
	done
	return 1
}
