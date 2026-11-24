/* A foreground MariaDB server with private, persistent initialization.
 * The data volume contains my.ini (client credentials), a database lock and
 * mariadb.initialized. No Windows service or host configuration is used.
 */
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <bcrypt.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <wchar.h>

#define CAP 32768
#define SERIES "12.3\n"

static wchar_t command[CAP];
static size_t used;

static int error(const wchar_t *message)
{
    fwprintf(stderr, L"mariadb-server: %ls (Windows error %lu)\n", message, GetLastError());
    return 1;
}

static void add(wchar_t ch)
{
    if (used + 1 >= CAP) {
        fputs("mariadb-server: command line is too long\n", stderr);
        exit(1);
    }
    command[used++] = ch;
    command[used] = 0;
}

/* Quote one argument using the Windows CRT's backslash/quote rules. */
static void argument(const wchar_t *value)
{
    if (used) add(L' ');
    add(L'"');
    while (*value) {
        size_t slashes = 0;
        while (*value == L'\\') { slashes++; value++; }
        size_t count = (*value == L'"' || !*value) ? 2 * slashes : slashes;
        while (count--) add(L'\\');
        if (!*value) break;
        if (*value == L'"') add(L'\\');
        add(*value++);
    }
    add(L'"');
}

static int path(wchar_t *dest, const wchar_t *dir, const wchar_t *name)
{
    if (wcslen(dir) + wcslen(name) + 2 >= CAP) return error(L"path is too long");
    wcscpy(dest, dir);
    wcscat(dest, L"\\");
    wcscat(dest, name);
    return 0;
}

static int option(const wchar_t *name, const wchar_t *value)
{
    wchar_t *text = malloc(CAP * sizeof(wchar_t));
    if (!text) return error(L"out of memory");
    if (wcslen(name) + wcslen(value) >= CAP) { free(text); return error(L"option is too long"); }
    wcscpy(text, name);
    wcscat(text, value);
    argument(text);
    free(text);
    return 0;
}

static int envpath(const wchar_t *name, wchar_t *dest)
{
    wchar_t *value = _wgetenv(name);
    if (!value || !*value) {
        fwprintf(stderr, L"mariadb-server: %ls must name a directory\n", name);
        return 1;
    }
    DWORD len = GetFullPathNameW(value, CAP, dest, NULL);
    if (!len || len >= CAP) return error(L"cannot resolve a data directory");
    while (len > 3 && (dest[len - 1] == L'\\' || dest[len - 1] == L'/')) dest[--len] = 0;
    return 0;
}

static int mkdirs(wchar_t *dir)
{
    if (CreateDirectoryW(dir, NULL)) return 0;
    DWORD code = GetLastError();
    if (code == ERROR_ALREADY_EXISTS) {
        DWORD attr = GetFileAttributesW(dir);
        if (attr != INVALID_FILE_ATTRIBUTES && (attr & FILE_ATTRIBUTE_DIRECTORY)) return 0;
    }
    if (code == ERROR_PATH_NOT_FOUND) {
        wchar_t *slash = wcsrchr(dir, L'\\');
        if (slash && slash > dir + 2) {
            *slash = 0;
            int result = mkdirs(dir);
            *slash = L'\\';
            if (result) return result;
            if (CreateDirectoryW(dir, NULL)) return 0;
        }
    }
    return error(L"cannot create the data directory");
}

/* 0 = empty, 1 = populated, -1 = cannot inspect. */
static int populated(const wchar_t *dir)
{
    wchar_t *pattern = malloc(CAP * sizeof(wchar_t));
    WIN32_FIND_DATAW entry;
    if (!pattern || path(pattern, dir, L"*")) { free(pattern); return -1; }
    HANDLE scan = FindFirstFileW(pattern, &entry);
    free(pattern);
    if (scan == INVALID_HANDLE_VALUE) {
        DWORD code = GetLastError();
        if (code == ERROR_FILE_NOT_FOUND || code == ERROR_PATH_NOT_FOUND) return 0;
        error(L"cannot inspect the database directory");
        return -1;
    }
    int result = 0;
    do {
        if (wcscmp(entry.cFileName, L".") && wcscmp(entry.cFileName, L"..")) {
            result = 1;
            break;
        }
    } while (FindNextFileW(scan, &entry));
    DWORD code = GetLastError();
    FindClose(scan);
    if (!result && code != ERROR_NO_MORE_FILES) return -1;
    return result;
}

static int write_new(const wchar_t *file, const char *text)
{
    HANDLE out = CreateFileW(file, GENERIC_WRITE, 0, NULL, CREATE_NEW, FILE_ATTRIBUTE_NORMAL, NULL);
    if (out == INVALID_HANDLE_VALUE) return error(L"cannot create initialization metadata");
    DWORD length = (DWORD)strlen(text), written;
    BOOL ok = WriteFile(out, text, length, &written, NULL) && written == length;
    if (ok) ok = FlushFileBuffers(out);
    CloseHandle(out);
    return ok ? 0 : error(L"cannot save initialization metadata");
}

static int credentials(const wchar_t *file, wchar_t *password)
{
    DWORD attr = GetFileAttributesW(file);
    if (attr != INVALID_FILE_ATTRIBUTES) {
        wchar_t saved[26];
        DWORD len = GetPrivateProfileStringW(L"client", L"password", L"", saved, 26, file);
        if (len != 24) {
            fputs("mariadb-server: existing my.ini has no generated password; initialization stopped\n", stderr);
            return 1;
        }
        for (DWORD i = 0; i < len; i++) {
            if (!((saved[i] >= L'A' && saved[i] <= L'Z') ||
                  (saved[i] >= L'a' && saved[i] <= L'z') ||
                  (saved[i] >= L'0' && saved[i] <= L'9'))) return 1;
        }
        wcscpy(password, saved);
        return 0;
    }
    static const char alphabet[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
    char secret[25];
    for (size_t i = 0; i < 24;) {
        unsigned char byte;
        if (BCryptGenRandom(NULL, &byte, 1, BCRYPT_USE_SYSTEM_PREFERRED_RNG) != 0) {
            fputs("mariadb-server: cannot generate a password\n", stderr);
            return 1;
        }
        if (byte >= 248) continue;
        secret[i] = alphabet[byte % 62];
        password[i] = (wchar_t)secret[i];
        i++;
    }
    secret[24] = 0;
    password[24] = 0;
    char ini[256];
    snprintf(ini, sizeof(ini), "[client]\nuser=root\npassword=%s\nhost=127.0.0.1\nprotocol=tcp\ndefault-character-set=utf8mb4\n", secret);
    return write_new(file, ini);
}

static BOOL WINAPI control(DWORD event)
{
    /* The child receives console events too; wait for its clean shutdown. */
    return event == CTRL_C_EVENT || event == CTRL_BREAK_EVENT;
}

static int run(const wchar_t *exe)
{
    STARTUPINFOW startup = { .cb = sizeof(startup) };
    PROCESS_INFORMATION process;
    DWORD result = 1;
    fflush(NULL);
    if (!CreateProcessW(exe, command, NULL, NULL, TRUE, 0, NULL, NULL, &startup, &process))
        return error(L"cannot start MariaDB");
    CloseHandle(process.hThread);
    if (WaitForSingleObject(process.hProcess, INFINITE) == WAIT_OBJECT_0)
        GetExitCodeProcess(process.hProcess, &result);
    else
        error(L"cannot wait for MariaDB");
    CloseHandle(process.hProcess);
    return (int)result;
}

int wmain(int argc, wchar_t **argv)
{
    /* Heap allocations avoid overflowing Windows' default 1 MiB stack. */
    wchar_t *basedir = calloc(CAP, sizeof(wchar_t));
    wchar_t *exe = calloc(CAP, sizeof(wchar_t));
    wchar_t *home = calloc(CAP, sizeof(wchar_t));
    wchar_t *data = calloc(CAP, sizeof(wchar_t));
    wchar_t *ini = calloc(CAP, sizeof(wchar_t));
    wchar_t *marker = calloc(CAP, sizeof(wchar_t));
    wchar_t *lockpath = calloc(CAP, sizeof(wchar_t));
    wchar_t *system = calloc(CAP, sizeof(wchar_t));
    wchar_t *plugins = calloc(CAP, sizeof(wchar_t));
    if (!basedir || !exe || !home || !data || !ini || !marker || !lockpath || !system || !plugins)
        return error(L"out of memory");
    DWORD len = GetModuleFileNameW(NULL, basedir, CAP);
    if (!len || len >= CAP) return error(L"cannot locate MariaDB");
    wchar_t *slash = wcsrchr(basedir, L'\\');
    if (!slash) return error(L"invalid executable directory");
    *slash = 0;
    if (path(exe, basedir, L"mariadbd.exe")) return 1;
    slash = wcsrchr(basedir, L'\\');
    if (!slash) return error(L"invalid image directory");
    *slash = 0;
    for (int i = 1; i < argc; i++) {
        if (!wcscmp(argv[i], L"--version") || !wcscmp(argv[i], L"-V") ||
            !wcscmp(argv[i], L"--help") || !wcscmp(argv[i], L"-?")) {
            argument(exe);
            argument(L"--no-defaults");
            for (int j = 1; j < argc; j++) argument(argv[j]);
            return run(exe);
        }
    }
    if (envpath(L"MARIADB_HOME", home) || envpath(L"MARIADB_DATADIR", data) || mkdirs(home)) return 1;
    if (path(ini, home, L"my.ini") || path(marker, home, L"mariadb.initialized") ||
        path(system, data, L"mysql")) return 1;
    /* Lock outside the database directory so an empty directory stays empty. */
    if (wcslen(data) + 16 >= CAP) return error(L"data path is too long");
    wcscpy(lockpath, data);
    wcscat(lockpath, L".quasidock.lock");
    HANDLE lock = CreateFileW(lockpath, GENERIC_READ | GENERIC_WRITE, 0, NULL, OPEN_ALWAYS,
                              FILE_ATTRIBUTE_NORMAL, NULL);
    if (lock == INVALID_HANDLE_VALUE) {
        fputs("mariadb-server: the data volume is already in use or cannot be locked\n", stderr);
        return 1;
    }
    FILE *state = _wfopen(marker, L"rb");
    if (state) {
        char series[16] = {0};
        size_t count = fread(series, 1, sizeof(series) - 1, state);
        fclose(state);
        if (count != strlen(SERIES) || strcmp(series, SERIES)) {
            fputs("mariadb-server: database version is incompatible; an explicit upgrade is required\n", stderr);
            return 1;
        }
        DWORD attr = GetFileAttributesW(system);
        if (attr == INVALID_FILE_ATTRIBUTES || !(attr & FILE_ATTRIBUTE_DIRECTORY) ||
            GetFileAttributesW(ini) == INVALID_FILE_ATTRIBUTES) {
            fputs("mariadb-server: initialized database or credentials are missing; refusing to recreate them\n", stderr);
            return 1;
        }
    } else {
        int contents = populated(data);
        if (contents != 0) {
            fputs("mariadb-server: database directory is nonempty without completed initialization; refusing to overwrite it\n", stderr);
            return 1;
        }
        wchar_t secret[25];
        if (credentials(ini, secret)) return 1;
        wchar_t *installer = calloc(CAP, sizeof(wchar_t));
        if (!installer || path(installer, basedir, L"bin\\mariadb-install-db.exe")) return 1;
        argument(installer);
        if (option(L"--datadir=", data) || option(L"--password=", secret)) return 1;
        argument(L"--silent");
        int code = run(installer);
        free(installer);
        if (code) {
            fputs("mariadb-server: initialization failed; existing files were preserved for inspection\n", stderr);
            return code;
        }
        if (write_new(marker, SERIES)) return 1;
    }
    used = 0;
    command[0] = 0;
    argument(exe);
    if (option(L"--defaults-file=", ini) || option(L"--basedir=", basedir) || option(L"--datadir=", data)) return 1;
    if (path(plugins, basedir, L"lib\\plugin") || option(L"--plugin-dir=", plugins)) return 1;
    wchar_t *port = _wgetenv(L"MYSQL_TCP_PORT");
    if (!port || !*port) port = L"3306";
    wchar_t *end;
    unsigned long number = wcstoul(port, &end, 10);
    if (*end || !number || number > 65535) {
        fputs("mariadb-server: MYSQL_TCP_PORT must be between 1 and 65535\n", stderr);
        return 1;
    }
    if (option(L"--port=", port)) return 1;
    argument(L"--bind-address=127.0.0.1");
    argument(L"--skip-name-resolve");
    argument(L"--console");
    wchar_t *temp = _wgetenv(L"TEMP");
    if (temp && *temp && option(L"--tmpdir=", temp)) return 1;
    for (int i = 1; i < argc; i++) argument(argv[i]);
    SetConsoleCtrlHandler(control, TRUE);
    int result = run(exe);
    CloseHandle(lock);
    return result;
}
