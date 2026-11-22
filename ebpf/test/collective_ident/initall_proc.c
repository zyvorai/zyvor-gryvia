// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

/* One fake process that creates 3 communicators with ncclCommInitAll (comm i has rank i) and runs one
 * allreduce on each. Used to test the ncclCommInitAll uretprobe (bpf_loop over the returned communicators). */
#include <stdlib.h>

typedef struct fakecomm *ncclComm_t;
int ncclCommInitAll(ncclComm_t *, int, const int *);
int ncclAllReduce(const void *, void *, size_t, int, int, ncclComm_t, void *);

int main(void)
{
	ncclComm_t comms[3];

	ncclCommInitAll(comms, 3, 0);
	for (int i = 0; i < 3; i++)
		ncclAllReduce(0, 0, 1024, 7, 0, comms[i], 0);
	return 0;
}
