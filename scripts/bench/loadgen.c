// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// loadgen: loopback load generator for scripts/bench-ebpf-overhead.sh.
//
// One process plays both ends of every test over 127.0.0.1, so it needs no second
// host. Each mode runs for -d seconds and prints one JSON object on stdout.
//
//   bulk      one TCP stream, 64 KiB writes            -> MB/s
//   conns     -n concurrent streams, 64 KiB writes     -> aggregate MB/s
//   connrate  -n threads doing connect/accept/close    -> connections/s
//   rr        one connection, 64 B request/response    -> latency percentiles (us)
//   syscall   getpid / read(/dev/zero,1) / write(/dev/null,1) -> ns per call
//
// Build: cc -O2 -pthread -o loadgen loadgen.c
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

static double dur = 3.0;
static int nconn = 32;
static atomic_int stop_flag;

static uint64_t now_ns(void) {
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
}

static void die(const char *m) {
	perror(m);
	exit(2);
}

static int listener(struct sockaddr_in *addr) {
	int l = socket(AF_INET, SOCK_STREAM, 0);
	if (l < 0) die("socket");
	int one = 1;
	setsockopt(l, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
	memset(addr, 0, sizeof *addr);
	addr->sin_family = AF_INET;
	addr->sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	addr->sin_port = 0;
	if (bind(l, (struct sockaddr *)addr, sizeof *addr) < 0) die("bind");
	socklen_t sl = sizeof *addr;
	getsockname(l, (struct sockaddr *)addr, &sl);
	if (listen(l, 1024) < 0) die("listen");
	return l;
}

static int dial(const struct sockaddr_in *addr) {
	int c = socket(AF_INET, SOCK_STREAM, 0);
	if (c < 0) die("socket");
	if (connect(c, (const struct sockaddr *)addr, sizeof *addr) < 0) die("connect");
	int one = 1;
	setsockopt(c, IPPROTO_TCP, TCP_NODELAY, &one, sizeof one);
	return c;
}

// ---- bulk / conns ----
#define CHUNK 65536
struct stream { int rfd, wfd; _Atomic uint64_t bytes; };

static void *sink(void *a) {
	struct stream *s = a;
	char *buf = malloc(CHUNK);
	for (;;) {
		ssize_t n = read(s->rfd, buf, CHUNK);
		if (n <= 0) break;
		atomic_fetch_add(&s->bytes, (uint64_t)n);
	}
	free(buf);
	return NULL;
}

static void *source(void *a) {
	struct stream *s = a;
	char *buf = calloc(1, CHUNK);
	while (!atomic_load(&stop_flag)) {
		if (write(s->wfd, buf, CHUNK) < 0) break;
	}
	shutdown(s->wfd, SHUT_WR);
	free(buf);
	return NULL;
}

static void run_streams(const char *mode, int n) {
	struct sockaddr_in addr;
	int l = listener(&addr);
	struct stream *st = calloc((size_t)n, sizeof *st);
	pthread_t *tid = calloc((size_t)n * 2, sizeof *tid);
	for (int i = 0; i < n; i++) {
		st[i].wfd = dial(&addr);
		st[i].rfd = accept(l, NULL, NULL);
		if (st[i].rfd < 0) die("accept");
	}
	uint64_t t0 = now_ns();
	for (int i = 0; i < n; i++) {
		pthread_create(&tid[2 * i], NULL, sink, &st[i]);
		pthread_create(&tid[2 * i + 1], NULL, source, &st[i]);
	}
	struct timespec ts = {(time_t)dur, (long)((dur - (time_t)dur) * 1e9)};
	nanosleep(&ts, NULL);
	atomic_store(&stop_flag, 1);
	for (int i = 0; i < 2 * n; i++) pthread_join(tid[i], NULL);
	double secs = (double)(now_ns() - t0) / 1e9;
	uint64_t total = 0;
	for (int i = 0; i < n; i++) total += atomic_load(&st[i].bytes);
	printf("{\"mode\":\"%s\",\"streams\":%d,\"seconds\":%.3f,\"mb_per_s\":%.1f}\n", mode, n, secs,
	       (double)total / 1e6 / secs);
}

// ---- connrate ----
static struct sockaddr_in cr_addr;
static int cr_listen;
static _Atomic uint64_t cr_count;

static void *cr_accept(void *a) {
	(void)a;
	while (!atomic_load(&stop_flag)) {
		int c = accept(cr_listen, NULL, NULL);
		if (c < 0) break;
		close(c);
	}
	return NULL;
}

static void *cr_client(void *a) {
	(void)a;
	while (!atomic_load(&stop_flag)) {
		int c = socket(AF_INET, SOCK_STREAM, 0);
		if (c < 0) break;
		if (connect(c, (struct sockaddr *)&cr_addr, sizeof cr_addr) == 0) atomic_fetch_add(&cr_count, 1);
		close(c);
	}
	return NULL;
}

static void run_connrate(int n) {
	cr_listen = listener(&cr_addr);
	pthread_t *tid = calloc((size_t)n * 2, sizeof *tid);
	uint64_t t0 = now_ns();
	for (int i = 0; i < n; i++) {
		pthread_create(&tid[2 * i], NULL, cr_accept, NULL);
		pthread_create(&tid[2 * i + 1], NULL, cr_client, NULL);
	}
	struct timespec ts = {(time_t)dur, (long)((dur - (time_t)dur) * 1e9)};
	nanosleep(&ts, NULL);
	atomic_store(&stop_flag, 1);
	double secs = (double)(now_ns() - t0) / 1e9;
	uint64_t c = atomic_load(&cr_count);
	// unblock accept(): shut the listener down, then join.
	shutdown(cr_listen, SHUT_RDWR);
	close(cr_listen);
	for (int i = 0; i < n; i++) pthread_join(tid[2 * i + 1], NULL);
	printf("{\"mode\":\"connrate\",\"threads\":%d,\"seconds\":%.3f,\"conn_per_s\":%.0f}\n", n, secs, (double)c / secs);
	fflush(stdout);
	_exit(0); // accept threads may still be parked; the numbers are printed
}

// ---- rr ----
static int cmp_u64(const void *a, const void *b) {
	uint64_t x = *(const uint64_t *)a, y = *(const uint64_t *)b;
	return x < y ? -1 : x > y;
}

static void *echo(void *a) {
	int fd = *(int *)a;
	char buf[64];
	for (;;) {
		ssize_t n = read(fd, buf, sizeof buf);
		if (n <= 0) break;
		if (write(fd, buf, (size_t)n) < 0) break;
	}
	return NULL;
}

static void run_rr(void) {
	struct sockaddr_in addr;
	int l = listener(&addr);
	int c = dial(&addr);
	int s = accept(l, NULL, NULL);
	int one = 1;
	setsockopt(s, IPPROTO_TCP, TCP_NODELAY, &one, sizeof one);
	pthread_t t;
	pthread_create(&t, NULL, echo, &s);
	size_t cap = 4u << 20, n = 0;
	uint64_t *lat = malloc(cap * sizeof *lat);
	char req[64] = {0}, rsp[64];
	uint64_t end = now_ns() + (uint64_t)(dur * 1e9);
	for (int i = 0; i < 2000; i++) { // warm-up, not recorded
		if (write(c, req, sizeof req) < 0 || read(c, rsp, sizeof rsp) <= 0) die("rr warmup");
	}
	while (now_ns() < end && n < cap) {
		uint64_t a = now_ns();
		if (write(c, req, sizeof req) < 0 || read(c, rsp, sizeof rsp) <= 0) die("rr");
		lat[n++] = now_ns() - a;
	}
	shutdown(c, SHUT_RDWR);
	qsort(lat, n, sizeof *lat, cmp_u64);
#define PCT(p) ((double)lat[(size_t)((double)(n - 1) * (p))] / 1000.0)
	printf("{\"mode\":\"rr\",\"requests\":%zu,\"p50_us\":%.2f,\"p99_us\":%.2f,\"p999_us\":%.2f,\"max_us\":%.2f}\n", n, PCT(0.50),
	       PCT(0.99), PCT(0.999), (double)lat[n - 1] / 1000.0);
}

// ---- syscall ----
static double loop_ns(int which) {
	char b = 0;
	int zero = open("/dev/zero", O_RDONLY), null = open("/dev/null", O_WRONLY);
	uint64_t calls = 0, t0 = now_ns(), end = t0 + (uint64_t)(dur * 1e9);
	volatile long sink_v = 0;
	while (now_ns() < end) {
		for (int i = 0; i < 20000; i++) {
			switch (which) {
			case 0: sink_v += syscall(SYS_getpid); break;
			case 1: if (read(zero, &b, 1) < 0) die("read"); break;
			default: if (write(null, &b, 1) < 0) die("write"); break;
			}
		}
		calls += 20000;
	}
	double ns = (double)(now_ns() - t0) / (double)calls;
	close(zero);
	close(null);
	(void)sink_v;
	return ns;
}

static void run_syscall(void) {
	double d = dur;
	dur = d / 3.0;
	double g = loop_ns(0), r = loop_ns(1), w = loop_ns(2);
	printf("{\"mode\":\"syscall\",\"getpid_ns\":%.1f,\"read_ns\":%.1f,\"write_ns\":%.1f}\n", g, r, w);
}

int main(int argc, char **argv) {
	const char *mode = argc > 1 ? argv[1] : "";
	for (int i = 2; i + 1 < argc; i += 2) {
		if (!strcmp(argv[i], "-d")) dur = atof(argv[i + 1]);
		else if (!strcmp(argv[i], "-n")) nconn = atoi(argv[i + 1]);
	}
	if (!strcmp(mode, "bulk")) run_streams("bulk", 1);
	else if (!strcmp(mode, "conns")) run_streams("conns", nconn);
	else if (!strcmp(mode, "connrate")) run_connrate(nconn);
	else if (!strcmp(mode, "rr")) run_rr();
	else if (!strcmp(mode, "syscall")) run_syscall();
	else {
		fprintf(stderr, "usage: loadgen bulk|conns|connrate|rr|syscall [-d seconds] [-n count]\n");
		return 64;
	}
	return 0;
}
