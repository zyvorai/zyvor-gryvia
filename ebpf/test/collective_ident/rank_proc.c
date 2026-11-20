// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

/* One fake "training rank": rank_proc <rank> <world> [late]
 * comm A (created first) and comm B (second): 3 allreduce fp32 on A (1 MiB),
 * 2 allgather bf16 on B, 1 failing alltoall on B, then UserRank on A, then
 * one more allreduce on A, destroy B.  "late" sleeps 1 s between init and the
 * collectives so a driver can attach the probes in between. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

typedef struct { char internal[128]; } ncclUniqueId;
typedef struct fakecomm *ncclComm_t;
int ncclCommInitRank(ncclComm_t *, int, ncclUniqueId, int);
int ncclCommUserRank(ncclComm_t, int *);
int ncclCommDestroy(ncclComm_t);
int ncclAllReduce(const void *, void *, size_t, int, int, ncclComm_t, void *);
int ncclAllGather(const void *, void *, size_t, int, ncclComm_t, void *);
int ncclAlltoAll(const void *, void *, size_t, int, ncclComm_t, void *);

int main(int argc, char **argv)
{
	int rank = atoi(argv[1]), world = atoi(argv[2]), r;
	int late = argc > 3 && !strcmp(argv[3], "late");
	ncclUniqueId id; ncclComm_t a, b;

	memset(&id, 7, sizeof(id));
	ncclCommInitRank(&a, world, id, rank);
	ncclCommInitRank(&b, world, id, rank);
	if (late) sleep(1);
	for (int i = 0; i < 3; i++) ncclAllReduce(0, 0, 262144, 7, 0, a, 0);
	for (int i = 0; i < 2; i++) ncclAllGather(0, 0, 1000, 9, b, 0);
	ncclAlltoAll(0, 0, 0xdead, 7, b, 0);
	ncclCommUserRank(a, &r);
	ncclAllReduce(0, 0, 262144, 7, 0, a, 0);
	ncclCommDestroy(b);
	return 0;
}
