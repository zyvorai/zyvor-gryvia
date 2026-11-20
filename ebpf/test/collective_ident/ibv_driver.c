// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

/* ibv_driver <ibv_verbs.o> <libfakeibv.so>: attach every probe, call the fake
 * exports in-process (this process is both traced and tracer), print the counters. */
#define _GNU_SOURCE
#include <bpf/libbpf.h>
#include <bpf/bpf.h>
#include <stdio.h>
#include <string.h>
#include <stdint.h>
#include <stdlib.h>
#include <unistd.h>
#include <sys/wait.h>

static int attach_all(struct bpf_object *obj, const char *lib)
{
	struct bpf_program *p; int n = 0;
	bpf_object__for_each_program(p, obj) {
		const char *sec = bpf_program__section_name(p);
		int ret = strncmp(sec, "uretprobe/", 10) == 0;
		if (!ret && strncmp(sec, "uprobe/", 7)) continue;
		LIBBPF_OPTS(bpf_uprobe_opts, o, .func_name = sec + (ret ? 10 : 7), .retprobe = ret);
		if (!bpf_program__attach_uprobe_opts(p, -1, lib, 0, &o)) { fprintf(stderr, "attach %s failed\n", sec); return -1; }
		n++;
	}
	printf("attached %d probes\n", n);
	return 0;
}

int main(int argc, char **argv)
{
	if (argc < 3) return 2;
	struct bpf_object *obj = bpf_object__open_file(argv[1], NULL);
	if (!obj || bpf_object__load(obj)) return 1;
	if (attach_all(obj, argv[2])) return 1;
	pid_t c = fork();
	if (!c) { setenv("LD_LIBRARY_PATH", ".", 1); execl("./ibv_proc", "ibv_proc", (char *)0); _exit(127); }
	waitpid(c, NULL, 0);
	int fd = bpf_map__fd(bpf_object__find_map_by_name(obj, "ibv_counts"));
	int ncpu = libbpf_num_possible_cpus();
	uint64_t *v = calloc(ncpu, sizeof(uint64_t));
	const char *names[] = {"qp_created", "qp_destroyed", "mr_registered", "mr_bytes"};
	for (uint32_t k = 0; k < 4; k++) {
		uint64_t sum = 0;
		if (bpf_map_lookup_elem(fd, &k, v)) { perror("lookup"); return 1; }
		for (int i = 0; i < ncpu; i++) sum += v[i];
		printf("%s=%llu\n", names[k], (unsigned long long)sum);
	}
	return 0;
}
