#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <linux/fs.h>
#include <linux/magic.h>
#include <linux/stat.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#include <sys/statfs.h>
#include <sys/syscall.h>
#include <unistd.h>

static const char data_root[] = "/var/fdb/data";
enum path_type { PATH_INVALID, PATH_DIRECTORY, PATH_REGULAR };
struct snapshot {
    struct stat st;
    uint64_t mount_id;
};

static enum path_type path_kind(const char *path)
{
    const char *relative, *leaf;
    size_t length;
    if (path == NULL)
        return PATH_INVALID;
    if (strcmp(path, data_root) == 0)
        return PATH_DIRECTORY;
    if (strncmp(path, data_root, sizeof(data_root) - 1) != 0 ||
        path[sizeof(data_root) - 1] != '/')
        return PATH_INVALID;
    relative = path + sizeof(data_root);
    if (strcmp(relative, "cluster") == 0)
        return PATH_DIRECTORY;
    if (strcmp(relative, "cluster/fdb.cluster") == 0 ||
        strcmp(relative, ".otel283-owner") == 0)
        return PATH_REGULAR;
    if (strncmp(relative, "processes", 9) == 0)
        length = 9;
    else if (strncmp(relative, "traces", 6) == 0)
        length = 6;
    else
        return PATH_INVALID;
    if (relative[length] == '\0')
        return PATH_DIRECTORY;
    if (relative[length] != '/')
        return PATH_INVALID;
    relative += length + 1;
    if (strncmp(relative, "1488", 4) != 0 ||
        relative[4] < '3' || relative[4] > '6')
        return PATH_INVALID;
    if (relative[5] == '\0')
        return PATH_DIRECTORY;
    if (relative[5] != '/')
        return PATH_INVALID;
    leaf = relative + 6;
    length = strlen(leaf);
    if (length == 0 || length > 255 || strcmp(leaf, ".") == 0 ||
        strcmp(leaf, "..") == 0)
        return PATH_INVALID;
    for (size_t i = 0; i < length; ++i) {
        unsigned char c = (unsigned char)leaf[i];
        if (!((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
              (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-'))
            return PATH_INVALID;
    }
    return PATH_REGULAR;
}

static bool allowed_mode(mode_t mode, enum path_type kind)
{
    return (kind == PATH_DIRECTORY && S_ISDIR(mode)) ||
           (kind == PATH_REGULAR && S_ISREG(mode));
}

static bool nocow_flag(uint32_t flags) { return (flags & FS_NOCOW_FL) != 0; }
static bool compression_flag(uint32_t flags) { return (flags & FS_COMPR_FL) != 0; }
static int cow_exit_status(uint32_t flags) { return nocow_flag(flags) ? 1 : 0; }

static int inspect_fd(int fd, enum path_type kind,
                      const struct snapshot *root, struct snapshot *out)
{
    struct statfs fs;
    struct statx sx;
    memset(&sx, 0, sizeof(sx));
    if (fstat(fd, &out->st) < 0)
        return -1;
    if (!allowed_mode(out->st.st_mode, kind)) {
        errno = EINVAL;
        return -1;
    }
    if (fstatfs(fd, &fs) < 0)
        return -1;
    if ((unsigned long)fs.f_type != (unsigned long)BTRFS_SUPER_MAGIC) {
        errno = EXDEV;
        return -1;
    }
    if (syscall(SYS_statx, fd, "", AT_EMPTY_PATH | AT_SYMLINK_NOFOLLOW,
                STATX_MNT_ID, &sx) < 0)
        return -1;
    if (!(sx.stx_mask & STATX_MNT_ID) || sx.stx_mnt_id == 0) {
        errno = EOPNOTSUPP;
        return -1;
    }
    out->mount_id = sx.stx_mnt_id;
    if (root != NULL && (out->st.st_dev != root->st.st_dev ||
                         out->mount_id != root->mount_id)) {
        errno = EXDEV;
        return -1;
    }
    return 0;
}

/* Open every parent explicitly: O_NOFOLLOW on just the final path is not enough. */
static int open_root(void)
{
    const char *components[] = { "var", "fdb", "data" };
    int fd = open("/", O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
    if (fd < 0)
        return -1;
    for (size_t i = 0; i < sizeof(components) / sizeof(components[0]); ++i) {
        int next = openat(fd, components[i],
                          O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
        int saved = errno;
        close(fd);
        if (next < 0) {
            errno = saved;
            return -1;
        }
        fd = next;
    }
    return fd;
}

static int open_target(const char *path, enum path_type kind,
                       struct snapshot *out)
{
    struct snapshot root;
    char relative[512];
    char *part, *next;
    int fd = open_root();
    if (fd < 0)
        return -1;
    if (inspect_fd(fd, PATH_DIRECTORY, NULL, &root) < 0)
        goto fail;
    if (strcmp(path, data_root) == 0) {
        *out = root;
        return fd;
    }
    /* Grammar bounds this to a fixed prefix plus a NAME_MAX-sized leaf. */
    if (strlen(path + sizeof(data_root)) >= sizeof(relative)) {
        errno = ENAMETOOLONG;
        goto fail;
    }
    strcpy(relative, path + sizeof(data_root));
    part = relative;
    do {
        int child;
        enum path_type expected;
        next = strchr(part, '/');
        if (next != NULL)
            *next = '\0';
        expected = next == NULL ? kind : PATH_DIRECTORY;
        child = openat(fd, part, O_RDONLY | O_NONBLOCK | O_NOFOLLOW | O_CLOEXEC |
                       (expected == PATH_DIRECTORY ? O_DIRECTORY : 0));
        if (child < 0)
            goto fail;
        close(fd);
        fd = child;
        if (inspect_fd(fd, expected, &root, out) < 0)
            goto fail;
        part = next == NULL ? NULL : next + 1;
    } while (part != NULL);
    return fd;
fail:
    {
        int saved = errno;
        close(fd);
        errno = saved;
        return -1;
    }
}

static int format_snapshot(char *buffer, size_t capacity, const char *path,
                           uint32_t flags, const struct snapshot *s)
{
    uintmax_t blocks;
    int length;
    if (buffer == NULL || s == NULL || path_kind(path) == PATH_INVALID ||
        !allowed_mode(s->st.st_mode, path_kind(path)) || s->mount_id == 0 ||
        s->st.st_size < 0 || s->st.st_blocks < 0) {
        errno = EINVAL;
        return -1;
    }
    blocks = (uintmax_t)s->st.st_blocks;
    if (blocks > UINTMAX_MAX / 512) {
        errno = EOVERFLOW;
        return -1;
    }
    length = snprintf(buffer, capacity,
        "{\"path\":\"%s\",\"flags\":%" PRIu32 ",\"nocow\":%s,"
        "\"compression_flag\":%s,\"uid\":%" PRIuMAX ",\"dev\":%" PRIuMAX ","
        "\"ino\":%" PRIuMAX ",\"nlink\":%" PRIuMAX ",\"size\":%" PRIuMAX ","
        "\"allocated_bytes\":%" PRIuMAX ",\"mount_id\":%" PRIu64 ","
        "\"filesystem\":\"btrfs\",\"nodatasum\":\"NOT_EXPOSED\"}\n",
        path, flags, nocow_flag(flags) ? "true" : "false",
        compression_flag(flags) ? "true" : "false", (uintmax_t)s->st.st_uid,
        (uintmax_t)s->st.st_dev, (uintmax_t)s->st.st_ino,
        (uintmax_t)s->st.st_nlink, (uintmax_t)s->st.st_size,
        blocks * 512, s->mount_id);
    if (length < 0)
        return -1;
    if ((size_t)length >= capacity) {
        errno = ENOSPC;
        return -1;
    }
    return length;
}

int main(int argc, char **argv)
{
    struct snapshot s;
    char output[1024];
    int fd, raw_flags = 0, length;
    enum path_type kind;
    if (argc != 2 || (kind = path_kind(argv[1])) == PATH_INVALID) {
        fprintf(stderr, "inode_flags: expected one allowed /var/fdb/data path (errno=%d)\n",
                EINVAL);
        return 1;
    }
    fd = open_target(argv[1], kind, &s);
    if (fd < 0)
        goto error;
    if (ioctl(fd, FS_IOC_GETFLAGS, &raw_flags) < 0) {
        int saved = errno;
        close(fd);
        errno = saved;
        goto error;
    }
    if (close(fd) < 0)
        goto error;
    length = format_snapshot(output, sizeof(output), argv[1],
                             (uint32_t)raw_flags, &s);
    if (length < 0)
        goto error;
    if (fwrite(output, 1, (size_t)length, stdout) != (size_t)length ||
        fflush(stdout) != 0) {
        if (errno == 0)
            errno = EIO;
        goto error;
    }
    return cow_exit_status((uint32_t)raw_flags);
error:
    {
        int saved = errno == 0 ? EIO : errno;
        fprintf(stderr, "inode_flags: %s (errno=%d)\n", strerror(saved), saved);
        return 1;
    }
}
