// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

/* driver <straggler.o> <libfakenccl.so> <normal|late> <world> <rank>...
 * Loads straggler.o with libbpf, attaches every uprobe/uretprobe by symbol to
 * the fake library, runs one rank_proc per rank (concurrently), then prints the
 * fabric_events ring as one line per record. */
#define _GNU_SOURCE
#include <bpf/libbpf.h>
#include <bpf/bpf.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/wait.h>
#include <stdint.h>

struct sig { uint64_t ts; uint32_t pid, cg; uint8_t type, op, pad[2]; uint32_t rank, world, peer;
	uint64_t lat, plat, bytes; uint32_t retry, rnr; char comm[16]; };

static int cnt;
static int on_ev(void *ctx, void *data, size_t sz)
{
	struct sig *s = data; (void)ctx;
	if (sz < sizeof(*s)) return 0;
	printf("EV pid=%u type=%u op=%u rank=%d world=%u ord=%u seq=%llu bytes=%llu flags=%u lat_ns=%llu comm=%s\n",
	       s->pid, s->type, s->op, (int)s->rank, s->world, s->peer, (unsigned long long)s->plat,
	       (unsigned long long)s->bytes, s->retry, (unsigned long long)s->lat, s->comm);
	cnt++;
	return 0;
}

static int attach_all(struct bpf_object *obj, const char *lib)
{
	struct bpf_program *p; int n = 0;
	bpf_object__for_each_program(p, obj) {
		const char *sec = bpf_program__section_name(p);
		int ret = strncmp(sec, "uretprobe/", 10) == 0;
		if (!ret && strncmp(sec, "uprobe/", 7)) continue;
		const char *sym = sec + (ret ? 10 : 7);
		LIBBPF_OPTS(bpf_uprobe_opts, o, .func_name = sym, .retprobe = ret);
		struct bpf_link *l = bpf_program__attach_uprobe_opts(p, -1, lib, 0, &o);
		if (!l) { fprintf(stderr, "attach %s failed\n", sec); return -1; }
		n++;
	}
	printf("attached %d probes\n", n);
	return 0;
}

int main(int argc, char **argv)
{
	if (argc < 6) return 2;
	const char *o = argv[1], *lib = argv[2]; int late = !strcmp(argv[3], "late");
	struct bpf_object *obj = bpf_object__open_file(o, NULL);
	if (!obj || bpf_object__load(obj)) { fprintf(stderr, "load failed\n"); return 1; }
	if (!late && attach_all(obj, lib)) return 1;
	int nr = argc - 5; pid_t pids[16];
	for (int i = 0; i < nr; i++) {
		pids[i] = fork();
		if (!pids[i]) {
			setenv("LD_LIBRARY_PATH", ".", 1);
			const char *proc = getenv("PROC") ? getenv("PROC") : "./rank_proc";
			execl(proc, proc, argv[5 + i], argv[4], late ? "late" : "run", (char *)0);
			_exit(127);
		}
	}
	if (late) { usleep(400000); if (attach_all(obj, lib)) return 1; }
	for (int i = 0; i < nr; i++) waitpid(pids[i], NULL, 0);
	struct ring_buffer *rb = ring_buffer__new(bpf_map__fd(bpf_object__find_map_by_name(obj, "fabric_events")), on_ev, NULL, NULL);
	ring_buffer__poll(rb, 200);
	printf("records=%d\n", cnt);
	return 0;
}
