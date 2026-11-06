/* SPDX-License-Identifier: MIT
 * Linux I/O adapter: directly linked into the bundled static PG18 clients,
 * or preloaded only into explicitly configured development workers.
 * Never preload this into PostgreSQL or DBS. Scope is checked against the
 * canonical /proc/self/fd path on every operation (including descriptor reuse).
 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <pthread.h>
#include <poll.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

#define MAGIC UINT64_C(0x5047313847554152)
#define SLOTS 16
#define MAX_FDS 8192
enum { M_MAGIC, M_VERSION, M_RBPS, M_WBPS, M_RNEXT, M_WNEXT,
       M_READ, M_WRITE, M_READ_ADVISED, M_WRITE_ADVISED, M_MINCORE_FAIL,
       M_ADVISE_FAIL, M_LOADS, M_ABORT, M_PRESERVED, M_SYNC_FAIL };
static int control_fd = -1;
static int supervisor_fd = -1;
static pid_t owner_pid;
static char control_path[PATH_MAX], read_roots[PATH_MAX * 4], write_roots[PATH_MAX * 4];
static size_t flush_window = 8U * 1024U * 1024U;
/* Optional for sequential verification. Per-read eviction plus FADV_RANDOM can
 * turn a scan into 4KiB physical requests. Batch completed ranges instead. */
static size_t read_window;
static size_t rate_burst;
static long page_size;
static int cache_enabled = 1;
static int socket_reads;
static pthread_mutex_t mutex = PTHREAD_MUTEX_INITIALIZER;
static __thread int entered;
struct dirty { dev_t dev; ino_t ino; off_t begin, end; int valid; };
static struct dirty pending[MAX_FDS];
/* Snapshot each file once. A snapshot before every read mistakes our own
 * asynchronous readahead for pre-existing business cache and retains it. */
struct resident { unsigned char *bits; size_t pages, bytes; off_t size; dev_t dev; ino_t ino; int attempted; };
static struct resident initial[MAX_FDS];
static struct dirty read_pending[MAX_FDS];
static size_t bitmap_bytes;
#define BITMAP_BUDGET (8U * 1024U * 1024U)

static int raw_close(int fd) { return (int)syscall(SYS_close, fd); }
static ssize_t raw_read(int fd, void *b, size_t n) { return syscall(SYS_read, fd, b, n); }
static ssize_t raw_write(int fd, const void *b, size_t n) { return syscall(SYS_write, fd, b, n); }
static int raw_fstat(int fd, struct stat *st) { return (int)syscall(SYS_fstat, fd, st); }
static uint64_t now_ns(void) { struct timespec t; clock_gettime(CLOCK_MONOTONIC, &t); return (uint64_t)t.tv_sec * 1000000000U + (uint64_t)t.tv_nsec; }

static int state_lock(uint64_t *s)
{
    if (!control_path[0]) return -1;
    if (owner_pid != getpid()) {
        if (control_fd >= 0) raw_close(control_fd);
        control_fd = (int)syscall(SYS_openat, AT_FDCWD, control_path, O_RDWR | O_CLOEXEC | O_NOFOLLOW, 0);
        owner_pid = getpid();
    }
    if (control_fd < 0) { errno = EIO; return -1; }
    struct flock lock = { .l_type = F_WRLCK, .l_whence = SEEK_SET };
    while (fcntl(control_fd, F_SETLKW, &lock) < 0) if (errno != EINTR) return -1;
    if (syscall(SYS_pread64, control_fd, s, SLOTS * 8, 0) != SLOTS * 8 || s[M_MAGIC] != MAGIC || s[M_VERSION] != 1) {
        lock.l_type = F_UNLCK; fcntl(control_fd, F_SETLK, &lock); errno = EIO; return -1;
    }
    return 0;
}
static int state_unlock(uint64_t *s)
{
    int result = syscall(SYS_pwrite64, control_fd, s, SLOTS * 8, 0) == SLOTS * 8 ? 0 : -1;
    struct flock lock = { .l_type = F_UNLCK, .l_whence = SEEK_SET };
    if (fcntl(control_fd, F_SETLK, &lock) < 0) result = -1;
    return result;
}
static void count_stat(int slot, uint64_t value)
{
    uint64_t s[SLOTS];
    if (state_lock(s) == 0) { s[slot] += value; (void)state_unlock(s); }
}
static int limit_io(size_t n, int writing)
{
    if (supervisor_fd >= 0) { struct pollfd p = {.fd=supervisor_fd,.events=POLLIN}; if (poll(&p,1,0) != 0) { errno=ECANCELED; return -1; } }
    uint64_t s[SLOTS], until = 0;
    if (state_lock(s) < 0) return -1; /* Fail closed if the control channel fails. */
    if (s[M_ABORT]) { (void)state_unlock(s); errno = ECANCELED; return -1; }
    unsigned rate_slot = writing ? M_WBPS : M_RBPS, next_slot = writing ? M_WNEXT : M_RNEXT;
    if (s[rate_slot]) {
        uint64_t now = now_ns();
        uint64_t credit = (uint64_t)(((__uint128_t)rate_burst * 1000000000U) / s[rate_slot]);
        uint64_t earliest = now > credit ? now-credit : 0;
        uint64_t base = s[next_slot] > earliest ? s[next_slot] : earliest;
        uint64_t duration = (uint64_t)(((__uint128_t)n * 1000000000U + s[rate_slot] - 1) / s[rate_slot]);
        until = base + duration; s[next_slot] = until;
    }
    if (state_unlock(s) < 0) { errno = EIO; return -1; }
    if (until) {
        struct timespec t = { .tv_sec = (time_t)(until / 1000000000U), .tv_nsec = (long)(until % 1000000000U) };
        int rc;
        do { rc = clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &t, NULL); } while (rc == EINTR);
        if (rc) { errno = rc; return -1; }
    }
    if (state_lock(s) < 0) return -1;
    int aborted = s[M_ABORT] != 0;
    if (state_unlock(s) < 0) return -1;
    if (aborted) { errno = ECANCELED; return -1; }
    if (supervisor_fd >= 0) { struct pollfd p = {.fd=supervisor_fd,.events=POLLIN}; if (poll(&p,1,0) != 0) { errno=ECANCELED; return -1; } }
    return 0;
}
static int under_roots(const char *path, const char *roots)
{
    while (*roots) {
        const char *end = strchr(roots, ':'); size_t n = end ? (size_t)(end - roots) : strlen(roots);
        if (n && !strncmp(path, roots, n) && (path[n] == '/' || path[n] == '\0')) return 1;
        if (!end) break;
        roots = end + 1;
    }
    return 0;
}
static int scoped(int fd, int writing, struct stat *st)
{
    if (control_fd == fd || raw_fstat(fd, st) || !S_ISREG(st->st_mode)) return 0;
    char link[64], path[PATH_MAX]; snprintf(link, sizeof(link), "/proc/self/fd/%d", fd);
    ssize_t n = readlink(link, path, sizeof(path) - 1);
    if (n <= 0 || (size_t)n >= sizeof(path) - 1) return 0;
    path[n] = '\0';
    return under_roots(path, writing ? write_roots : read_roots);
}
static void advise(int fd, off_t begin, off_t length, int slot)
{
    if (length <= 0) return;
    int rc = posix_fadvise(fd, begin, length, POSIX_FADV_DONTNEED);
    if (rc) count_stat(M_ADVISE_FAIL, 1);
    else count_stat(slot, (uint64_t)length); /* Requested bytes, NOT measured eviction. */
}
static struct resident snapshot(int fd, off_t off, size_t n, const struct stat *st)
{
    struct resident empty = {0};
    if (fd < 0 || fd >= MAX_FDS || off < 0 || st->st_size <= 0 || !n) return empty;
    struct resident *r = &initial[fd];
    if (r->attempted && r->dev == st->st_dev && r->ino == st->st_ino) return *r;
    if (r->bits) { bitmap_bytes -= r->bytes; free(r->bits); }
    *r = (struct resident){ .size = st->st_size, .dev = st->st_dev, .ino = st->st_ino, .attempted = 1 };
    r->pages = ((uint64_t)st->st_size + (size_t)page_size - 1) / (size_t)page_size;
    r->bytes = (r->pages + 7) / 8;
    if (r->bytes > BITMAP_BUDGET - bitmap_bytes) { count_stat(M_MINCORE_FAIL, 1); return *r; }
    r->bits = calloc(r->bytes, 1);
    /* mmap without touching pages does not read the file into cache. Old
     * qemu-user incorrectly rejects valid PROT_NONE mincore mappings. */
    void *map = mmap(NULL, r->pages * (size_t)page_size, PROT_READ, MAP_SHARED, fd, 0);
    int failed = !r->bits || map == MAP_FAILED;
    unsigned char vec[4096];
    for (size_t p = 0; !failed && p < r->pages; p += sizeof(vec)) {
        size_t count = r->pages - p < sizeof(vec) ? r->pages - p : sizeof(vec);
        if (mincore((char *)map + p * (size_t)page_size, count * (size_t)page_size, vec)) { failed = 1; break; }
        for (size_t i = 0; i < count; i++) if (vec[i] & 1) r->bits[(p+i)/8] |= (unsigned char)(1U << ((p+i)%8));
    }
    if (map != MAP_FAILED) munmap(map, r->pages * (size_t)page_size);
    if (failed) { free(r->bits); r->bits = NULL; count_stat(M_MINCORE_FAIL, 1); }
    else bitmap_bytes += r->bytes;
    return *r;
}
static void read_pattern_once(int fd, const struct stat *st)
{
    if (fd >= 0 && fd < MAX_FDS && initial[fd].attempted && initial[fd].dev == st->st_dev && initial[fd].ino == st->st_ino) return;
    (void)posix_fadvise(fd, 0, 0, read_window ? POSIX_FADV_SEQUENTIAL : POSIX_FADV_RANDOM);
}
static void drop_new_pages(int fd, off_t off, size_t n, struct resident r)
{
    if (!r.bits) return; /* Residency unknown: preserve rather than evict blindly. */
    off_t end = off + (off_t)n, run = -1;
    uint64_t preserved = 0;
    size_t first = (size_t)(off / page_size), last = ((size_t)end + (size_t)page_size - 1) / (size_t)page_size;
    if (last > r.pages) last = r.pages;
    for (size_t i = first; i < last; i++) {
        off_t p = (off_t)i * page_size;
        int full = p >= off && (p + page_size <= end || (end >= r.size && p < r.size));
        int resident = r.bits[i/8] & (1U << (i%8));
        int cold = full && !resident;
        if (full && resident) preserved += (uint64_t)page_size;
        if (cold && run < 0) run = p;
        if (!cold && run >= 0) { advise(fd, run, p - run, M_READ_ADVISED); run = -1; }
    }
    if (run >= 0) advise(fd, run, (off_t)last * page_size - run, M_READ_ADVISED);
    if (preserved) count_stat(M_PRESERVED, preserved);
}
static void flush_read(int fd)
{
    if (fd < 0 || fd >= MAX_FDS || !read_pending[fd].valid) return;
    struct dirty *p = &read_pending[fd];
    struct stat st;
    if (!raw_fstat(fd, &st) && st.st_dev == p->dev && st.st_ino == p->ino)
        drop_new_pages(fd, p->begin, (size_t)(p->end-p->begin), initial[fd]);
    p->valid = 0;
}
static void queue_read(int fd, off_t off, size_t n, struct resident r)
{
    if (!read_window) { drop_new_pages(fd, off, n, r); return; }
    if (fd < 0 || fd >= MAX_FDS || off < 0) return;
    struct dirty *p = &read_pending[fd];
    if (p->valid && (p->dev != r.dev || p->ino != r.ino || off != p->end)) flush_read(fd);
    if (!p->valid) *p = (struct dirty){ .dev=r.dev, .ino=r.ino, .begin=off, .end=off, .valid=1 };
    p->end = off + (off_t)n;
    if ((uint64_t)(p->end-p->begin) >= read_window) flush_read(fd);
}
static int flush_dirty(int fd, int already_synced)
{
    if (!cache_enabled) return 0;
    if (fd < 0 || fd >= MAX_FDS || !pending[fd].valid) return 0;
    struct dirty *d = &pending[fd]; struct stat st;
    if (raw_fstat(fd, &st) || st.st_dev != d->dev || st.st_ino != d->ino) { d->valid = 0; return 0; }
    if (!already_synced && syscall(SYS_fdatasync, fd) < 0) { count_stat(M_SYNC_FAIL, 1); return -1; }
    off_t begin = d->begin - d->begin % page_size;
    off_t end = ((d->end + page_size - 1) / page_size) * page_size;
    advise(fd, begin, end - begin, M_WRITE_ADVISED); d->valid = 0;
    return 0;
}
static int mark_write(int fd, off_t off, ssize_t n, const struct stat *st)
{
    count_stat(M_WRITE, (uint64_t)n);
    if (!cache_enabled) return 0;
    if (fd < 0 || fd >= MAX_FDS || off < 0) { errno = EMFILE; return -1; }
    struct dirty *d = &pending[fd];
    if (!d->valid || d->dev != st->st_dev || d->ino != st->st_ino)
        *d = (struct dirty){ .dev = st->st_dev, .ino = st->st_ino, .begin = off, .end = off + n, .valid = 1 };
    else { if (off < d->begin) d->begin = off; if (off + n > d->end) d->end = off + n; }
    return (uint64_t)(d->end - d->begin) >= flush_window ? flush_dirty(fd, 0) : 0;
}
__attribute__((constructor)) static void initialize(void)
{
    const char *v = getenv("PG18GUARD_CONTROL");
    if (!v || strlen(v) >= sizeof(control_path)) return;
    strcpy(control_path, v);
    v = getenv("PG18GUARD_READ_ROOTS"); if (v && strlen(v) < sizeof(read_roots)) strcpy(read_roots, v);
    v = getenv("PG18GUARD_WRITE_ROOTS"); if (v && strlen(v) < sizeof(write_roots)) strcpy(write_roots, v);
    v = getenv("PG18GUARD_FLUSH_BYTES"); if (v) { unsigned long long n = strtoull(v, NULL, 10); if (n >= 4096) flush_window = (size_t)n; }
    v = getenv("PG18GUARD_READ_WINDOW_BYTES"); if (v) { unsigned long long n = strtoull(v, NULL, 10); if (n >= 4096 && n <= 64U*1024U*1024U) read_window = (size_t)n; }
    v = getenv("PG18GUARD_RATE_BURST_BYTES"); if (v) { unsigned long long n = strtoull(v, NULL, 10); if (n <= 64U*1024U*1024U) rate_burst = (size_t)n; }
    v = getenv("PG18GUARD_CACHE_POLICY"); if (v && !strcmp(v, "off")) cache_enabled = 0;
    v = getenv("PG18GUARD_SOCKET_READS"); if (v && !strcmp(v, "1")) socket_reads = 1;
    page_size = sysconf(_SC_PAGESIZE); if (page_size <= 0) page_size = 4096;
    v = getenv("PG18GUARD_SUPERVISOR_PID");
    if (v) {
        long pid = strtol(v, NULL, 10);
        if (pid > 1) supervisor_fd = (int)syscall(SYS_pidfd_open, (pid_t)pid, 0);
        if (supervisor_fd < 0) { /* Refuse guarded I/O if required liveness cannot be established. */
            entered = 1; uint64_t s[SLOTS]; if (state_lock(s)==0) {s[M_ABORT]=1;(void)state_unlock(s);} entered=0;
        }
    }
    entered = 1; pthread_mutex_lock(&mutex); count_stat(M_LOADS, 1); pthread_mutex_unlock(&mutex); entered = 0;
}
/* Opt-in only for the native pg_basebackup client. libpq's raw read uses recv;
 * share one receive budget across the base stream and forked WAL receiver.
 * This is logical receive pacing, not a physical device I/O limit. */
ssize_t recv(int fd, void *buf, size_t n, int flags)
{
    ssize_t result = syscall(SYS_recvfrom, fd, buf, n, flags, NULL, NULL);
    if (result <= 0 || entered || !socket_reads || !control_path[0]) return result;
    entered = 1; pthread_mutex_lock(&mutex);
    if (limit_io((size_t)result, 0)) result = -1;
    else count_stat(M_READ, (uint64_t)result);
    int saved = errno; pthread_mutex_unlock(&mutex); entered = 0; errno = saved;
    return result;
}
ssize_t read(int fd, void *buf, size_t n)
{
    if (entered || !control_path[0]) return raw_read(fd, buf, n);
    entered = 1; pthread_mutex_lock(&mutex); struct stat st; ssize_t result;
    if (!scoped(fd, 0, &st)) result = raw_read(fd, buf, n);
    else {
        off_t off = lseek(fd, 0, SEEK_CUR);
        size_t charge = off >= 0 && off < st.st_size ? (uint64_t)(st.st_size-off) < n ? (size_t)(st.st_size-off) : n : 0;
        if (limit_io(charge,0)) result=-1;
        else {
            struct resident r = {0};
            if (cache_enabled) { read_pattern_once(fd, &st); r = snapshot(fd, off, n, &st); }
            result = raw_read(fd, buf, n);
            if (result > 0) { count_stat(M_READ, (uint64_t)result); if (cache_enabled) queue_read(fd, off, (size_t)result, r); }
        }
    }
    int saved = errno; pthread_mutex_unlock(&mutex); entered = 0; errno = saved; return result;
}
ssize_t pread64(int fd, void *buf, size_t n, off_t off)
{
    if (entered || !control_path[0]) return syscall(SYS_pread64, fd, buf, n, off);
    entered = 1; pthread_mutex_lock(&mutex); struct stat st; ssize_t result;
    if (!scoped(fd, 0, &st)) result = syscall(SYS_pread64, fd, buf, n, off);
    else {
        size_t charge = off >= 0 && off < st.st_size ? (uint64_t)(st.st_size-off) < n ? (size_t)(st.st_size-off) : n : 0;
        if (limit_io(charge,0)) result=-1;
        else {
            struct resident r = {0};
            if (cache_enabled) { read_pattern_once(fd, &st); r = snapshot(fd, off, n, &st); }
            result = syscall(SYS_pread64, fd, buf, n, off);
            if (result > 0) { count_stat(M_READ, (uint64_t)result); if (cache_enabled) queue_read(fd, off, (size_t)result, r); }
        }
    }
    int saved = errno; pthread_mutex_unlock(&mutex); entered = 0; errno = saved; return result;
}
ssize_t write(int fd, const void *buf, size_t n)
{
    if (entered || !control_path[0]) return raw_write(fd, buf, n);
    entered = 1; pthread_mutex_lock(&mutex); struct stat st; ssize_t result;
    if (!scoped(fd, 1, &st)) result = raw_write(fd, buf, n);
    else if (limit_io(n, 1)) result = -1;
    else { off_t off = lseek(fd, 0, SEEK_CUR); result = raw_write(fd, buf, n); if (result > 0 && mark_write(fd, off, result, &st)) result = -1; }
    int saved = errno; pthread_mutex_unlock(&mutex); entered = 0; errno = saved; return result;
}
ssize_t pwrite64(int fd, const void *buf, size_t n, off_t off)
{
    if (entered || !control_path[0]) return syscall(SYS_pwrite64, fd, buf, n, off);
    entered = 1; pthread_mutex_lock(&mutex); struct stat st; ssize_t result;
    if (!scoped(fd, 1, &st)) result = syscall(SYS_pwrite64, fd, buf, n, off);
    else if (limit_io(n, 1)) result = -1;
    else { result = syscall(SYS_pwrite64, fd, buf, n, off); if (result > 0 && mark_write(fd, off, result, &st)) result = -1; }
    int saved = errno; pthread_mutex_unlock(&mutex); entered = 0; errno = saved; return result;
}
ssize_t pread(int fd, void *buf, size_t n, off_t off) { return pread64(fd, buf, n, off); }
ssize_t pwrite(int fd, const void *buf, size_t n, off_t off) { return pwrite64(fd, buf, n, off); }
int fsync(int fd)
{
    if (entered || !control_path[0]) return (int)syscall(SYS_fsync, fd);
    entered = 1; pthread_mutex_lock(&mutex); int result = (int)syscall(SYS_fsync, fd);
    if (!result && flush_dirty(fd, 1)) result = -1;
    int saved = errno; pthread_mutex_unlock(&mutex); entered = 0; errno = saved; return result;
}
int fdatasync(int fd)
{
    if (entered || !control_path[0]) return (int)syscall(SYS_fdatasync, fd);
    entered = 1; pthread_mutex_lock(&mutex); int result = (int)syscall(SYS_fdatasync, fd);
    if (!result && flush_dirty(fd, 1)) result = -1;
    int saved = errno; pthread_mutex_unlock(&mutex); entered = 0; errno = saved; return result;
}
int close(int fd)
{
    if (entered || !control_path[0]) return raw_close(fd);
    entered = 1; pthread_mutex_lock(&mutex); flush_read(fd); int flush = flush_dirty(fd, 0), saved = errno;
    int result = raw_close(fd); if (flush) { result = -1; errno = saved; }
    if (fd >= 0 && fd < MAX_FDS) pending[fd].valid = 0;
    if (fd >= 0 && fd < MAX_FDS) { if (initial[fd].bits) { bitmap_bytes -= initial[fd].bytes; free(initial[fd].bits); } initial[fd] = (struct resident){0}; }
    saved = errno; pthread_mutex_unlock(&mutex); entered = 0; errno = saved; return result;
}

#ifdef PGCACHEGUARD_STATIC
/* Link with --wrap=fread,--wrap=fwrite,--wrap=fflush,--wrap=fclose. Static libc
 * stdio uses private syscall paths; wrapping read/write alone misses fwrite.
 * These adapters keep official PostgreSQL option parsing and file formats. */
extern size_t __real_fread(void *, size_t, size_t, FILE *);
extern size_t __real_fwrite(const void *, size_t, size_t, FILE *);
extern int __real_fflush(FILE *);
extern int __real_fclose(FILE *);

size_t __wrap_fread(void *ptr, size_t size, size_t count, FILE *stream)
{
    if (entered || !control_path[0] || !size || !count) return __real_fread(ptr,size,count,stream);
    if (count > SIZE_MAX / size) { errno=EOVERFLOW; return 0; }
    entered=1; pthread_mutex_lock(&mutex);
    int fd=fileno(stream); struct stat st; size_t result;
    if (!scoped(fd,0,&st)) result=__real_fread(ptr,size,count,stream);
    else {
        off_t off=ftello(stream); size_t n=size*count;
        if (off<0) { errno=EIO; result=0; }
        else {
            size_t charge=off<st.st_size ? ((uint64_t)(st.st_size-off)<n ? (size_t)(st.st_size-off) : n) : 0;
            if (limit_io(charge,0)) result=0;
            else {
                struct resident r={0};
                if (cache_enabled) { read_pattern_once(fd,&st); r=snapshot(fd,off,n,&st); }
                result=__real_fread(ptr,size,count,stream);
                /* ftello includes a final incomplete item; count actual bytes. */
                off_t end=ftello(stream);
                size_t actual=end>=off ? (size_t)(end-off) : result*size;
                count_stat(M_READ,(uint64_t)actual);
                if (cache_enabled && actual) queue_read(fd,off,actual,r);
            }
        }
    }
    int saved=errno; pthread_mutex_unlock(&mutex); entered=0; errno=saved; return result;
}
size_t __wrap_fwrite(const void *ptr, size_t size, size_t count, FILE *stream)
{
    if (entered || !control_path[0] || !size || !count) return __real_fwrite(ptr,size,count,stream);
    if (count > SIZE_MAX / size) { errno=EOVERFLOW; return 0; }
    entered=1; pthread_mutex_lock(&mutex);
    int fd=fileno(stream); struct stat st; size_t result;
    if (!scoped(fd,1,&st)) result=__real_fwrite(ptr,size,count,stream);
    else if (limit_io(size*count,1)) result=0;
    else {
        off_t off=ftello(stream);
        if (off<0) { errno=EIO; result=0; }
        else {
            result=__real_fwrite(ptr,size,count,stream);
            if (result && (__real_fflush(stream) || mark_write(fd,off,(ssize_t)(result*size),&st))) result=0;
        }
    }
    int saved=errno; pthread_mutex_unlock(&mutex); entered=0; errno=saved; return result;
}
int __wrap_fflush(FILE *stream)
{
    if (entered || !control_path[0]) return __real_fflush(stream);
    entered=1; pthread_mutex_lock(&mutex);
    int result=__real_fflush(stream);
    if (!result) {
        if (stream) result=flush_dirty(fileno(stream),0);
        else for (int fd=0;fd<MAX_FDS;fd++) if (flush_dirty(fd,0)) { result=-1; break; }
    }
    int saved=errno; pthread_mutex_unlock(&mutex); entered=0; errno=saved; return result;
}
int __wrap_fclose(FILE *stream)
{
    if (entered || !control_path[0]) return __real_fclose(stream);
    entered=1; pthread_mutex_lock(&mutex);
    int fd=fileno(stream), result=__real_fflush(stream), saved=errno;
    flush_read(fd);
    if (!result && flush_dirty(fd,0)) { result=-1; saved=errno; }
    if (__real_fclose(stream) && !result) { result=-1; saved=errno; }
    if (fd>=0 && fd<MAX_FDS) {
        pending[fd].valid=0;
        if (initial[fd].bits) { bitmap_bytes-=initial[fd].bytes; free(initial[fd].bits); }
        initial[fd]=(struct resident){0};
    }
    pthread_mutex_unlock(&mutex); entered=0; errno=saved; return result;
}
#endif
