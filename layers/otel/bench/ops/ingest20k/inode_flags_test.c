#define main inode_flags_cli_main
#include "inode_flags.c"
#undef main
#include <stdlib.h>

static unsigned checks;
#define CHECK(condition) do { \
    ++checks; \
    if (!(condition)) { \
        fprintf(stderr, "line %d: %s\n", __LINE__, #condition); \
        exit(1); \
    } \
} while (0)

static void test_paths(void)
{
    const char *valid_files[] = {
        "/var/fdb/data/.otel283-owner", "/var/fdb/data/cluster/fdb.cluster",
        "/var/fdb/data/processes/14883/AZaz09_.-",
        "/var/fdb/data/traces/14886/.hidden", "/var/fdb/data/traces/14884/..."
    };
    const char *invalid[] = {
        "", "/", "/var/fdb", "/var/fdb/data/", "/var/fdb/data-other",
        "/var/fdb/datax/processes", "/var/fdb/data//processes",
        "/var/fdb/data/../data", "/var/fdb/data/cluster/", "/var/fdb/data/clusterx",
        "/var/fdb/data/cluster/fdb.cluster/extra", "/var/fdb/data/.otel283-owner/",
        "/var/fdb/data/tracesx", "/var/fdb/data/processesx/14883/x",
        "/var/fdb/data/processes/", "/var/fdb/data/processes/1488",
        "/var/fdb/data/processes/14882", "/var/fdb/data/processes/14887",
        "/var/fdb/data/processes/148830", "/var/fdb/data/traces/14883/",
        "/var/fdb/data/traces/14883/.", "/var/fdb/data/traces/14883/..",
        "/var/fdb/data/traces/14883/a/b", "/var/fdb/data/traces/14883/a b",
        "/var/fdb/data/traces/14883/a\"b", "/var/fdb/data/traces/14883/a\\b",
        "/var/fdb/data/traces/14883/a\nb", "/var/fdb/data/traces/14883/\303\251"
    };
    char path[512];
    const char *dirs[] = { "processes", "traces" };
    CHECK(path_kind(NULL) == PATH_INVALID);
    CHECK(path_kind("/var/fdb/data") == PATH_DIRECTORY);
    CHECK(path_kind("/var/fdb/data/cluster") == PATH_DIRECTORY);
    for (size_t i = 0; i < sizeof(dirs) / sizeof(dirs[0]); ++i) {
        snprintf(path, sizeof(path), "/var/fdb/data/%s", dirs[i]);
        CHECK(path_kind(path) == PATH_DIRECTORY);
        for (unsigned port = 14883; port <= 14886; ++port) {
            snprintf(path, sizeof(path), "/var/fdb/data/%s/%u", dirs[i], port);
            CHECK(path_kind(path) == PATH_DIRECTORY);
            snprintf(path, sizeof(path), "/var/fdb/data/%s/%u/file", dirs[i], port);
            CHECK(path_kind(path) == PATH_REGULAR);
        }
    }
    for (size_t i = 0; i < sizeof(valid_files) / sizeof(valid_files[0]); ++i)
        CHECK(path_kind(valid_files[i]) == PATH_REGULAR);
    for (size_t i = 0; i < sizeof(invalid) / sizeof(invalid[0]); ++i)
        CHECK(path_kind(invalid[i]) == PATH_INVALID);
    strcpy(path, "/var/fdb/data/traces/14883/");
    size_t prefix = strlen(path);
    memset(path + prefix, 'x', 255);
    path[prefix + 255] = '\0';
    CHECK(path_kind(path) == PATH_REGULAR);
    path[prefix + 255] = 'x';
    path[prefix + 256] = '\0';
    CHECK(path_kind(path) == PATH_INVALID);
}

static void test_types_and_flags(void)
{
    const mode_t modes[] = { S_IFREG, S_IFDIR, S_IFLNK, S_IFIFO,
                             S_IFSOCK, S_IFCHR, S_IFBLK, 0 };
    for (size_t i = 0; i < sizeof(modes) / sizeof(modes[0]); ++i) {
        CHECK(allowed_mode(modes[i] | 0600, PATH_REGULAR) == (modes[i] == S_IFREG));
        CHECK(allowed_mode(modes[i] | 0700, PATH_DIRECTORY) == (modes[i] == S_IFDIR));
        CHECK(!allowed_mode(modes[i], PATH_INVALID));
    }
    const uint32_t flags[] = { 0, FS_COMPR_FL, FS_NOCOW_FL,
                               FS_COMPR_FL | FS_NOCOW_FL, UINT32_MAX };
    for (size_t i = 0; i < sizeof(flags) / sizeof(flags[0]); ++i) {
        CHECK(nocow_flag(flags[i]) == ((flags[i] & FS_NOCOW_FL) != 0));
        CHECK(compression_flag(flags[i]) == ((flags[i] & FS_COMPR_FL) != 0));
        CHECK(cow_exit_status(flags[i]) == ((flags[i] & FS_NOCOW_FL) ? 1 : 0));
    }
    CHECK(cow_exit_status(0) == 0);
    CHECK(cow_exit_status(FS_COMPR_FL) == 0);
    CHECK(cow_exit_status(FS_NOCOW_FL) == 1);
    CHECK(nocow_flag(FS_NOCOW_FL));
    CHECK(!compression_flag(FS_NOCOW_FL));
    CHECK(!nocow_flag(FS_COMPR_FL));
    CHECK(compression_flag(FS_COMPR_FL));
    CHECK(cow_exit_status(FS_IMMUTABLE_FL) == 0);
}

static void test_output(void)
{
    const char *path = "/var/fdb/data/traces/14883/x";
    char output[1024];
    struct snapshot s;
    memset(&s, 0, sizeof(s));
    s.st.st_mode = S_IFREG | 0600;
    s.st.st_dev = 12;
    s.st.st_ino = 34;
    s.st.st_nlink = 1;
    s.st.st_size = 567;
    s.st.st_blocks = 8;
    s.mount_id = 90;
    const char *expected =
        "{\"path\":\"/var/fdb/data/traces/14883/x\",\"flags\":0,\"nocow\":false,"
        "\"compression_flag\":false,\"uid\":0,\"dev\":12,\"ino\":34,\"nlink\":1,"
        "\"size\":567,\"allocated_bytes\":4096,\"mount_id\":90,"
        "\"filesystem\":\"btrfs\",\"nodatasum\":\"NOT_EXPOSED\"}\n";
    int length = format_snapshot(output, sizeof(output), path, 0, &s);
    CHECK(length == (int)strlen(expected));
    CHECK(strcmp(output, expected) == 0);
    CHECK(format_snapshot(output, (size_t)length + 1, path, 0, &s) == length);
    CHECK(format_snapshot(output, (size_t)length, path, 0, &s) == -1 && errno == ENOSPC);
    CHECK(format_snapshot(output, 0, path, 0, &s) == -1 && errno == ENOSPC);
    CHECK(format_snapshot(NULL, sizeof(output), path, 0, &s) == -1 && errno == EINVAL);
    CHECK(format_snapshot(output, sizeof(output), path, 0, NULL) == -1 && errno == EINVAL);
    CHECK(format_snapshot(output, sizeof(output), "/bad", 0, &s) == -1 && errno == EINVAL);
    CHECK(format_snapshot(output, sizeof(output), "/var/fdb/data", 0, &s) == -1 && errno == EINVAL);
    s.mount_id = 0;
    CHECK(format_snapshot(output, sizeof(output), path, 0, &s) == -1 && errno == EINVAL);
    s.mount_id = UINT64_MAX;
    s.st.st_size = -1;
    CHECK(format_snapshot(output, sizeof(output), path, 0, &s) == -1 && errno == EINVAL);
    s.st.st_size = 0;
    s.st.st_blocks = -1;
    CHECK(format_snapshot(output, sizeof(output), path, 0, &s) == -1 && errno == EINVAL);
    s.st.st_blocks = (blkcnt_t)(UINTMAX_MAX / 512 + 1);
    CHECK(format_snapshot(output, sizeof(output), path, 0, &s) == -1 && errno == EOVERFLOW);
    s.st.st_blocks = 0;
    s.st.st_uid = 1000;
    CHECK(format_snapshot(output, sizeof(output), path, UINT32_MAX, &s) > 0);
    CHECK(strstr(output, "\"flags\":4294967295,\"nocow\":true,\"compression_flag\":true") != NULL);
    CHECK(strstr(output, "\"uid\":1000,") != NULL);
    CHECK(strstr(output, "\"size\":0,\"allocated_bytes\":0,\"mount_id\":18446744073709551615") != NULL);
    s.st.st_mode = S_IFDIR | 0700;
    CHECK(format_snapshot(output, sizeof(output), "/var/fdb/data", FS_COMPR_FL, &s) > 0);
    CHECK(strstr(output, "\"nocow\":false,\"compression_flag\":true") != NULL);
}

int main(void)
{
    test_paths();
    test_types_and_flags();
    test_output();
    CHECK(checks >= 100);
    printf("inode_flags: %u pure checks passed\n", checks);
    return 0;
}
