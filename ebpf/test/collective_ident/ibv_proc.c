// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

#include <stddef.h>
void *ibv_create_qp(void *, void *); int ibv_destroy_qp(void *);
void *ibv_reg_mr(void *, void *, size_t, int); void *ibv_reg_mr_iova2(void *, void *, size_t, unsigned long, int);
int main(void)
{
	char buf[8]; void *q;
	q = ibv_create_qp(0, buf); ibv_create_qp(0, buf); ibv_create_qp(0, 0);   /* 2 ok, 1 failed */
	ibv_destroy_qp(q); ibv_destroy_qp(0);                                      /* 1 ok, 1 failed */
	ibv_reg_mr(0, buf, 4096, 0); ibv_reg_mr(0, 0, 999999, 0);                  /* 1 ok (4096 B), 1 failed */
	ibv_reg_mr_iova2(0, buf, 1 << 20, 0, 0);                                   /* 1 ok (1 MiB) */
	return 0;
}
