// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

/* Stand-in for libnccl.so: exports the NCCL symbols straggler.c probes, with the
 * real public signatures (ncclUniqueId passed by value, 128 bytes) but no GPU
 * and no communication.  The only behaviour: a collective sleeps
 * FAKE_NCCL_DELAY_US microseconds (env) * (seq on that comm) if the rank equals
 * FAKE_NCCL_SLOW_RANK, so a "slow rank" is observable.  NOT real NCCL. */
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

typedef struct { char internal[128]; } ncclUniqueId;
struct fakecomm { int rank, nranks; };
typedef struct fakecomm *ncclComm_t;

#define API __attribute__((noinline, visibility("default")))

API int ncclCommInitRank(ncclComm_t *comm, int nranks, ncclUniqueId id, int myrank)
{
	(void)id;
	struct fakecomm *c = malloc(sizeof(*c));
	c->rank = myrank; c->nranks = nranks;
	*comm = c;
	return 0;
}

API int ncclCommInitRankConfig(ncclComm_t *comm, int nranks, ncclUniqueId id, int myrank, void *config)
{
	(void)config;
	return ncclCommInitRank(comm, nranks, id, myrank);
}

API int ncclCommInitAll(ncclComm_t *comms, int ndev, const int *devlist)
{
	(void)devlist;
	for (int i = 0; i < ndev; i++) {
		comms[i] = malloc(sizeof(struct fakecomm));
		comms[i]->rank = i; comms[i]->nranks = ndev;
	}
	return 0;
}

API int ncclCommUserRank(ncclComm_t comm, int *rank) { *rank = comm->rank; return 0; }
API int ncclCommCount(ncclComm_t comm, int *n) { *n = comm->nranks; return 0; }
API int ncclCommDestroy(ncclComm_t comm) { free(comm); return 0; }
API int ncclCommAbort(ncclComm_t comm) { free(comm); return 0; }

static void maybe_sleep(ncclComm_t comm)
{
	const char *slow = getenv("FAKE_NCCL_SLOW_RANK"), *us = getenv("FAKE_NCCL_DELAY_US");
	if (slow && us && atoi(slow) == comm->rank)
		usleep(atoi(us));
}

API int ncclAllReduce(const void *s, void *r, size_t count, int dt, int op, ncclComm_t comm, void *stream)
{
	(void)s; (void)r; (void)count; (void)dt; (void)op; (void)stream;
	maybe_sleep(comm);
	return 0;
}
API int ncclBroadcast(const void *s, void *r, size_t count, int dt, int root, ncclComm_t comm, void *stream)
{
	(void)s; (void)r; (void)count; (void)dt; (void)root; (void)comm; (void)stream;
	return 0;
}
API int ncclAllGather(const void *s, void *r, size_t count, int dt, ncclComm_t comm, void *stream)
{
	(void)s; (void)r; (void)count; (void)dt; (void)comm; (void)stream;
	return 0;
}
API int ncclReduceScatter(const void *s, void *r, size_t count, int dt, int op, ncclComm_t comm, void *stream)
{
	(void)s; (void)r; (void)count; (void)dt; (void)op; (void)comm; (void)stream;
	return 0;
}
/* Forced failure: count == 0xdead returns ncclInvalidArgument (4). */
API int ncclAlltoAll(const void *s, void *r, size_t count, int dt, ncclComm_t comm, void *stream)
{
	(void)s; (void)r; (void)dt; (void)comm; (void)stream;
	return count == 0xdead ? 4 : 0;
}
