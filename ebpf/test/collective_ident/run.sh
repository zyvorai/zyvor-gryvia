#!/bin/sh
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Host-side test of ebpf/straggler.c with a stand-in libnccl (see fakenccl.c).
# Also tests ebpf/ibv_verbs.c against a stand-in libibverbs (fakeibv.c).
# Needs root, clang/gcc and libbpf headers.  Usage: run.sh <path to straggler.o> [<path to ibv_verbs.o>]
set -e
cd "$(dirname "$0")"
OBJ=${1:-../../straggler.o}
gcc -O0 -g -shared -fPIC -o libfakenccl.so fakenccl.c
gcc -O0 -g -o rank_proc rank_proc.c -L. -lfakenccl
gcc -O0 -g -o driver driver.c -lbpf
echo "== attach before init, 2 ranks of a world of 4 (rank 1 delayed)"
sudo FAKE_NCCL_SLOW_RANK=1 FAKE_NCCL_DELAY_US=20000 ./driver "$OBJ" ./libfakenccl.so normal 4 0 1
echo "== attach after init (late)"
sudo ./driver "$OBJ" ./libfakenccl.so late 4 2
echo "== ncclCommInitAll: 3 communicators of one process, ranks 0,1,2, ordinals 1,2,3"
gcc -O0 -g -o initall_proc initall_proc.c -L. -lfakenccl
OUT=$(sudo PROC=./initall_proc ./driver "$OBJ" ./libfakenccl.so normal 3 0)
echo "$OUT"
for r in 0 1 2; do
	echo "$OUT" | grep -q "rank=$r world=3 ord=$((r + 1)) seq=1 " || { echo "FAIL: ncclCommInitAll rank $r"; exit 1; }
done
rm -f driver rank_proc initall_proc libfakenccl.so
IBV=${2:-../../ibv_verbs.o}
if [ -f "$IBV" ]; then
	echo "== ibv_verbs.o: expect qp_created=2 qp_destroyed=1 mr_registered=2 mr_bytes=1052672"
	gcc -O0 -g -shared -fPIC -o libfakeibv.so fakeibv.c
	gcc -O0 -g -o ibv_proc ibv_proc.c -L. -lfakeibv
	gcc -O0 -g -o ibv_driver ibv_driver.c -lbpf
	sudo ./ibv_driver "$IBV" ./libfakeibv.so
	rm -f ibv_driver ibv_proc libfakeibv.so
fi
