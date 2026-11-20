// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

/* Stand-in for libibverbs.so control-path symbols (unversioned).  NOT rdma-core. */
#include <stddef.h>
#define API __attribute__((noinline, visibility("default")))
static int dummy[4];
API void *ibv_create_qp(void *pd, void *attr) { (void)pd; (void)attr; return attr ? &dummy[0] : NULL; }
API int ibv_destroy_qp(void *qp) { return qp ? 0 : 22; }
API void *ibv_reg_mr(void *pd, void *addr, size_t length, int access) { (void)pd; (void)length; (void)access; return addr ? &dummy[1] : NULL; }
API void *ibv_reg_mr_iova2(void *pd, void *addr, size_t length, unsigned long iova, int access) { (void)pd; (void)length; (void)iova; (void)access; return addr ? &dummy[2] : NULL; }
