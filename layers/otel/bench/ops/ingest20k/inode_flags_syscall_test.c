#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <linux/fs.h>
#include <linux/magic.h>
#include <linux/stat.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#include <sys/statfs.h>
#include <sys/syscall.h>
#include <unistd.h>

static int fake_open(const char *, int, ...);
static int fake_openat(int, const char *, int, ...);
static int fake_fstat(int, struct stat *);
static int fake_fstatfs(int, struct statfs *);
static long fake_syscall(long, ...);
static int fake_ioctl(int, unsigned long, ...);
static int fake_close(int);
static size_t fake_fwrite(const void *, size_t, size_t, FILE *);
static int fake_fflush(FILE *);
static int fake_fprintf(FILE *, const char *, ...);
#define open fake_open
#define openat fake_openat
#define fstat fake_fstat
#define fstatfs fake_fstatfs
#define syscall fake_syscall
#define ioctl fake_ioctl
#define close fake_close
#define fwrite fake_fwrite
#define fflush fake_fflush
#define fprintf fake_fprintf
#define main inode_flags_cli_main
#include "inode_flags.c"
#undef main
#undef open
#undef openat
#undef fstat
#undef fstatfs
#undef syscall
#undef ioctl
#undef close
#undef fwrite
#undef fflush
#undef fprintf

static unsigned checks;
#define CHECK(condition) do { \
    ++checks; \
    if (!(condition)) { \
        fprintf(stderr, "line %d: %s\n", __LINE__, #condition); \
        exit(1); \
    } \
} while (0)

enum fault {
    NONE, OPEN_ERROR, OPENAT_ERROR, PARENT_SYMLINK, STAT_ERROR, WRONG_TYPE,
    STATFS_ERROR, NOT_BTRFS, STATX_ERROR, MISSING_MOUNT_MASK, ZERO_MOUNT_ID,
    CROSS_DEVICE, CROSS_MOUNT, IOCTL_ERROR, CLOSE_ERROR, WRITE_ERROR,
    FLUSH_ERROR, NEGATIVE_SIZE, NEGATIVE_BLOCKS, BLOCK_OVERFLOW,
    DIRECTORY_AS_FILE, FILE_AS_DIRECTORY
};
struct fake_fd { char path[512]; bool live; };
static struct {
    enum fault fault;
    unsigned at, openats, stats, statfs_calls, statx_calls, ioctls, closes;
    unsigned writes, flushes, errors;
    int next_fd, error_errno;
    uint32_t flags;
    char output[2048];
    size_t output_length;
    struct fake_fd fds[32];
} state;

static void reset(enum fault fault, unsigned at, uint32_t flags)
{
    memset(&state, 0, sizeof(state));
    state.fault = fault;
    state.at = at;
    state.flags = flags;
    state.next_fd = 10;
    errno = 0;
}

static void check_fd(int fd)
{
    CHECK(fd >= 10 && fd < state.next_fd);
    CHECK(state.fds[fd].live);
}

static void check_open_flags(int flags)
{
    CHECK((flags & O_ACCMODE) == O_RDONLY);
    CHECK((flags & O_NOFOLLOW) != 0);
    CHECK((flags & O_CLOEXEC) != 0);
    CHECK((flags & (O_CREAT | O_TRUNC | O_APPEND)) == 0);
}

static int fake_open(const char *path, int flags, ...)
{
    CHECK(strcmp(path, "/") == 0);
    check_open_flags(flags);
    CHECK((flags & O_DIRECTORY) != 0);
    if (state.fault == OPEN_ERROR) {
        errno = EACCES;
        return -1;
    }
    int fd = state.next_fd++;
    strcpy(state.fds[fd].path, path);
    state.fds[fd].live = true;
    return fd;
}

static int fake_openat(int parent, const char *name, int flags, ...)
{
    check_fd(parent);
    check_open_flags(flags);
    CHECK(strchr(name, '/') == NULL);
    CHECK(strcmp(name, ".") != 0 && strcmp(name, "..") != 0);
    ++state.openats;
    if (state.openats <= 3) {
        const char *names[] = { "var", "fdb", "data" };
        CHECK(strcmp(name, names[state.openats - 1]) == 0);
        CHECK((flags & O_DIRECTORY) != 0);
    } else {
        CHECK((flags & O_NONBLOCK) != 0);
    }
    if ((state.fault == OPENAT_ERROR && state.openats == state.at) ||
        (state.fault == PARENT_SYMLINK && strcmp(name, "traces") == 0)) {
        errno = state.fault == PARENT_SYMLINK ? ELOOP : EACCES;
        return -1;
    }
    int fd = state.next_fd++;
    CHECK(fd < 32);
    size_t prefix = strlen(state.fds[parent].path);
    size_t suffix = strlen(name);
    bool slash = prefix > 1;
    CHECK(prefix + (slash ? 1 : 0) + suffix < sizeof(state.fds[fd].path));
    memcpy(state.fds[fd].path, state.fds[parent].path, prefix);
    if (slash)
        state.fds[fd].path[prefix++] = '/';
    memcpy(state.fds[fd].path + prefix, name, suffix + 1);
    state.fds[fd].live = true;
    enum path_type kind = path_kind(state.fds[fd].path);
    if (kind == PATH_REGULAR)
        CHECK((flags & O_DIRECTORY) == 0);
    else
        CHECK((flags & O_DIRECTORY) != 0);
    return fd;
}

static const char *checkpoint_paths[] = {
    "/var/fdb/data/processes/14883/serverCheckpoints",
    "/var/fdb/data/processes/14883/fetchedCheckpoints",
    "/var/fdb/data/processes/14886/serverCheckpoints",
    "/var/fdb/data/processes/14886/fetchedCheckpoints"
};

static int fake_fstat(int fd, struct stat *st)
{
    check_fd(fd);
    ++state.stats;
    if (state.fault == STAT_ERROR && state.stats == state.at) {
        errno = EIO;
        return -1;
    }
    memset(st, 0, sizeof(*st));
    st->st_mode = path_kind(state.fds[fd].path) == PATH_REGULAR ? S_IFREG : S_IFDIR;
    for (size_t i = 0; i < sizeof(checkpoint_paths) / sizeof(checkpoint_paths[0]); ++i) {
        if (strcmp(state.fds[fd].path, checkpoint_paths[i]) == 0)
            st->st_mode = S_IFDIR; /* Native fixture kind, independent of the production grammar. */
    }
    st->st_mode |= 0600;
    st->st_uid = 0; /* UID zero is data, not a rejected ownership policy. */
    st->st_dev = 12;
    st->st_ino = 34;
    st->st_nlink = 1;
    st->st_size = 567;
    st->st_blocks = 8;
    if (state.stats == state.at) {
        if (state.fault == WRONG_TYPE)
            st->st_mode = S_IFLNK;
        if (state.fault == DIRECTORY_AS_FILE)
            st->st_mode = S_IFREG;
        if (state.fault == FILE_AS_DIRECTORY)
            st->st_mode = S_IFDIR;
        if (state.fault == CROSS_DEVICE)
            st->st_dev = 13;
        if (state.fault == NEGATIVE_SIZE)
            st->st_size = -1;
        if (state.fault == NEGATIVE_BLOCKS)
            st->st_blocks = -1;
        if (state.fault == BLOCK_OVERFLOW)
            st->st_blocks = (blkcnt_t)(UINTMAX_MAX / 512 + 1);
    }
    return 0;
}

static int fake_fstatfs(int fd, struct statfs *fs)
{
    check_fd(fd);
    ++state.statfs_calls;
    if (state.fault == STATFS_ERROR && state.statfs_calls == state.at) {
        errno = EIO;
        return -1;
    }
    memset(fs, 0, sizeof(*fs));
    fs->f_type = state.fault == NOT_BTRFS && state.statfs_calls == state.at
        ? 0xEF53 : BTRFS_SUPER_MAGIC;
    return 0;
}

static long fake_syscall(long number, ...)
{
    va_list args;
    CHECK(number == SYS_statx);
    va_start(args, number);
    int fd = va_arg(args, int);
    const char *path = va_arg(args, const char *);
    int flags = va_arg(args, int);
    unsigned mask = va_arg(args, unsigned);
    struct statx *sx = va_arg(args, struct statx *);
    va_end(args);
    check_fd(fd);
    CHECK(strcmp(path, "") == 0);
    CHECK(flags == (AT_EMPTY_PATH | AT_SYMLINK_NOFOLLOW));
    CHECK(mask == STATX_MNT_ID);
    ++state.statx_calls;
    if (state.fault == STATX_ERROR && state.statx_calls == state.at) {
        errno = ENOSYS;
        return -1;
    }
    CHECK(sx->stx_mask == 0 && sx->stx_mnt_id == 0);
    sx->stx_mask = STATX_MNT_ID;
    sx->stx_mnt_id = 90;
    if (state.statx_calls == state.at) {
        if (state.fault == MISSING_MOUNT_MASK)
            sx->stx_mask = 0;
        if (state.fault == ZERO_MOUNT_ID)
            sx->stx_mnt_id = 0;
        if (state.fault == CROSS_MOUNT)
            sx->stx_mnt_id = 91;
    }
    return 0;
}

static int fake_ioctl(int fd, unsigned long request, ...)
{
    va_list args;
    check_fd(fd);
    CHECK(request == FS_IOC_GETFLAGS); /* Any setter request fails this test. */
    CHECK(state.stats == state.statfs_calls && state.stats == state.statx_calls);
    va_start(args, request);
    int *flags = va_arg(args, int *);
    va_end(args);
    ++state.ioctls;
    if (state.fault == IOCTL_ERROR) {
        errno = ENOTTY;
        return -1;
    }
    *flags = (int)state.flags;
    return 0;
}

static int fake_close(int fd)
{
    check_fd(fd);
    ++state.closes;
    state.fds[fd].live = false;
    if (state.fault == CLOSE_ERROR && state.ioctls != 0) {
        errno = EIO;
        return -1;
    }
    return 0;
}

static size_t fake_fwrite(const void *data, size_t size, size_t count, FILE *stream)
{
    CHECK(stream == stdout && size == 1);
    ++state.writes;
    if (state.fault == WRITE_ERROR) {
        errno = EIO;
        return 0;
    }
    CHECK(count < sizeof(state.output));
    memcpy(state.output, data, count);
    state.output[count] = '\0';
    state.output_length = count;
    return count;
}

static int fake_fflush(FILE *stream)
{
    CHECK(stream == stdout);
    ++state.flushes;
    if (state.fault == FLUSH_ERROR) {
        errno = 0; /* Exercise the fallback EIO branch. */
        return EOF;
    }
    return 0;
}

static int fake_fprintf(FILE *stream, const char *format, ...)
{
    CHECK(stream == stderr);
    CHECK(strstr(format, "inode_flags:") != NULL);
    va_list args;
    va_start(args, format);
    if (strstr(format, "expected one") == NULL) {
        const char *message = va_arg(args, const char *);
        CHECK(message != NULL && message[0] != '\0');
    }
    state.error_errno = va_arg(args, int);
    va_end(args);
    CHECK(state.error_errno > 0);
    ++state.errors;
    return 0;
}

static int run(const char *path)
{
    char *argv[] = { (char *)"inode-flags", (char *)path, NULL };
    int result = inode_flags_cli_main(2, argv);
    for (int fd = 10; fd < state.next_fd; ++fd)
        CHECK(!state.fds[fd].live);
    return result;
}

static void test_success(void)
{
    const char *paths[] = { "/var/fdb/data", "/var/fdb/data/.otel283-owner",
        "/var/fdb/data/cluster/fdb.cluster", "/var/fdb/data/traces",
        "/var/fdb/data/traces/14883", "/var/fdb/data/traces/14883/x",
        "/var/fdb/data/processes/14886/x", "/var/fdb/data/cluster",
        "/var/fdb/data/processes/14883/serverCheckpoints",
        "/var/fdb/data/processes/14883/fetchedCheckpoints",
        "/var/fdb/data/processes/14886/serverCheckpoints",
        "/var/fdb/data/processes/14886/fetchedCheckpoints" };
    const uint32_t flags[] = { 0, FS_COMPR_FL, FS_NOCOW_FL, FS_NOCOW_FL | FS_COMPR_FL };
    for (size_t p = 0; p < sizeof(paths) / sizeof(paths[0]); ++p) {
        for (size_t f = 0; f < sizeof(flags) / sizeof(flags[0]); ++f) {
            reset(NONE, 0, flags[f]);
            CHECK(run(paths[p]) == (f >= 2 ? 1 : 0));
            CHECK(state.errors == 0);
            CHECK(state.ioctls == 1 && state.writes == 1 && state.flushes == 1);
            CHECK(state.output_length == strlen(state.output));
            CHECK(state.output[0] == '{' && state.output[state.output_length - 1] == '\n');
            CHECK(strstr(state.output, paths[p]) != NULL);
            CHECK(strstr(state.output, f >= 2 ? "\"nocow\":true" : "\"nocow\":false") != NULL);
            CHECK(strstr(state.output, f % 2 ? "\"compression_flag\":true" : "\"compression_flag\":false") != NULL);
            CHECK(strstr(state.output, "\"uid\":0,\"dev\":12,\"ino\":34,\"nlink\":1,\"size\":567,\"allocated_bytes\":4096,\"mount_id\":90") != NULL);
            CHECK(strstr(state.output, "\"filesystem\":\"btrfs\",\"nodatasum\":\"NOT_EXPOSED\"}") != NULL);
            CHECK(state.stats == state.statfs_calls && state.stats == state.statx_calls);
            if (p == 0)
                CHECK(state.stats == 1 && state.openats == 3);
            if (p == 5)
                CHECK(state.stats == 4 && state.openats == 6);
        }
    }
}

static void test_failed_reads(void)
{
    const enum fault inspect_faults[] = { STAT_ERROR, WRONG_TYPE, STATFS_ERROR,
        NOT_BTRFS, STATX_ERROR, MISSING_MOUNT_MASK, ZERO_MOUNT_ID };
    const char *path = "/var/fdb/data/traces/14883/x";
    const enum fault checkpoint_faults[] = { IOCTL_ERROR, WRONG_TYPE, DIRECTORY_AS_FILE,
        CROSS_DEVICE, CROSS_MOUNT, STATX_ERROR };
    for (size_t p = 0; p < sizeof(checkpoint_paths) / sizeof(checkpoint_paths[0]); ++p) {
        for (size_t f = 0; f < sizeof(checkpoint_faults) / sizeof(checkpoint_faults[0]); ++f) {
            reset(checkpoint_faults[f], 4, 0);
            CHECK(run(checkpoint_paths[p]) != 0);
            CHECK(state.errors == 1 && state.output_length == 0);
            CHECK(state.ioctls == (checkpoint_faults[f] == IOCTL_ERROR ? 1U : 0U));
        }
    }
    const char *wrong_checkpoint_dirs[] = {
        "/var/fdb/data/processes/14884/serverCheckpoints",
        "/var/fdb/data/processes/14885/fetchedCheckpoints",
        "/var/fdb/data/traces/14883/serverCheckpoints",
        "/var/fdb/data/processes/14883/serverCheckpoint",
        "/var/fdb/data/processes/14886/fetchedCheckpoints-extra"
    };
    for (size_t p = 0; p < sizeof(wrong_checkpoint_dirs) / sizeof(wrong_checkpoint_dirs[0]); ++p) {
        reset(FILE_AS_DIRECTORY, 4, 0);
        CHECK(run(wrong_checkpoint_dirs[p]) != 0);
        CHECK(state.errors == 1 && state.ioctls == 0 && state.output_length == 0);
    }
    reset(IOCTL_ERROR, 0, 0);
    CHECK(run("/var/fdb/data/cluster") != 0 && state.ioctls == 1);
    CHECK(state.errors == 1 && state.output_length == 0);
    reset(NONE, 0, FS_NOCOW_FL);
    CHECK(run("/var/fdb/data/cluster") == 1 && state.ioctls == 1);
    CHECK(state.errors == 0 && strstr(state.output, "\"nocow\":true") != NULL);
    CHECK(strstr(state.output, "\"path\":\"/var/fdb/data/cluster\"") != NULL);
    for (size_t i = 0; i < sizeof(inspect_faults) / sizeof(inspect_faults[0]); ++i) {
        for (unsigned at = 1; at <= 4; ++at) {
            reset(inspect_faults[i], at, 0);
            CHECK(run(path) != 0);
            CHECK(state.errors == 1 && state.ioctls == 0);
            CHECK(state.writes == 0 && state.output_length == 0);
        }
    }
    reset(DIRECTORY_AS_FILE, 1, 0);
    CHECK(run(path) != 0 && state.ioctls == 0 && state.output_length == 0);
    reset(DIRECTORY_AS_FILE, 3, 0);
    CHECK(run(path) != 0 && state.ioctls == 0 && state.output_length == 0);
    reset(FILE_AS_DIRECTORY, 4, 0);
    CHECK(run(path) != 0 && state.ioctls == 0 && state.output_length == 0);
    for (unsigned at = 2; at <= 4; ++at) {
        reset(CROSS_DEVICE, at, 0);
        CHECK(run(path) != 0 && state.ioctls == 0 && state.output_length == 0);
        reset(CROSS_MOUNT, at, 0);
        CHECK(run(path) != 0 && state.ioctls == 0 && state.output_length == 0);
    }
    for (unsigned at = 1; at <= 6; ++at) {
        reset(OPENAT_ERROR, at, 0);
        CHECK(run(path) != 0 && state.ioctls == 0 && state.output_length == 0);
    }
    const enum fault remaining[] = { OPEN_ERROR, PARENT_SYMLINK, IOCTL_ERROR, CLOSE_ERROR,
        NEGATIVE_SIZE, NEGATIVE_BLOCKS, BLOCK_OVERFLOW, WRITE_ERROR, FLUSH_ERROR };
    for (size_t i = 0; i < sizeof(remaining) / sizeof(remaining[0]); ++i) {
        reset(remaining[i], 4, 0);
        CHECK(run(path) != 0 && state.errors == 1);
        if (remaining[i] != FLUSH_ERROR)
            CHECK(state.output_length == 0);
        else {
            CHECK(state.writes == 1 && state.flushes == 1);
            CHECK(state.error_errno == EIO);
        }
        if (remaining[i] == OPEN_ERROR || remaining[i] == PARENT_SYMLINK)
            CHECK(state.ioctls == 0);
        else
            CHECK(state.ioctls == 1);
    }
}

static void test_cli_and_bound(void)
{
    char *argv[] = { (char *)"inode-flags", (char *)"/var/fdb/data", (char *)"extra", NULL };
    for (int argc = 0; argc <= 3; ++argc) {
        if (argc == 2)
            continue;
        reset(NONE, 0, 0);
        CHECK(inode_flags_cli_main(argc, argv) != 0);
        CHECK(state.next_fd == 10 && state.errors == 1 && state.output_length == 0);
    }
    reset(NONE, 0, 0);
    CHECK(run("/var/fdb/data/../elsewhere") != 0);
    CHECK(state.next_fd == 10 && state.errors == 1 && state.output_length == 0);
    char oversized[800];
    struct snapshot s;
    strcpy(oversized, "/var/fdb/data/");
    memset(oversized + 14, 'x', sizeof(oversized) - 15);
    oversized[sizeof(oversized) - 1] = '\0';
    reset(NONE, 0, 0);
    CHECK(open_target(oversized, PATH_REGULAR, &s) == -1 && errno == ENAMETOOLONG);
    CHECK(state.ioctls == 0 && state.output_length == 0);
    for (int fd = 10; fd < state.next_fd; ++fd)
        CHECK(!state.fds[fd].live);
}

int main(void)
{
    test_success();
    test_failed_reads();
    test_cli_and_bound();
    CHECK(checks >= 1000);
    printf("inode_flags_syscall: %u injected checks passed\n", checks);
    return 0;
}
