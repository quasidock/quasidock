/* Native entrypoint: create container configuration and wait for NGINX.
 * Workers launch the upstream executable directly, without regenerating files.
 */
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <wchar.h>

#define CAP 32768
static wchar_t command[CAP];
static size_t used;

static void fail(const char *message)
{
    fprintf(stderr, "nginx: %s (Windows error %lu)\n", message, GetLastError());
    exit(1);
}

static void *alloc(size_t size)
{
    void *p = calloc(1, size);
    if (!p) fail("out of memory");
    return p;
}

static wchar_t *join(const wchar_t *dir, const wchar_t *name)
{
    size_t size = wcslen(dir) + wcslen(name) + 2;
    if (size >= CAP) fail("path is too long");
    wchar_t *p = alloc(size * sizeof(wchar_t));
    swprintf(p, size, L"%ls/%ls", dir, name);
    return p;
}

static wchar_t *absolute(const wchar_t *value)
{
    wchar_t *p = alloc(CAP * sizeof(wchar_t));
    DWORD n = GetFullPathNameW(value, CAP, p, NULL);
    if (!n || n >= CAP) fail("cannot resolve path");
    for (DWORD i = 0; i < n; i++) if (p[i] == L'\\') p[i] = L'/';
    while (n > 3 && p[n - 1] == L'/') p[--n] = 0;
    return p;
}

static const wchar_t *env(const wchar_t *name)
{
    const wchar_t *p = _wgetenv(name);
    return p && *p ? p : NULL;
}

static void mkdirs(const wchar_t *dir)
{
    if (CreateDirectoryW(dir, NULL)) return;
    DWORD code = GetLastError();
    if (code == ERROR_ALREADY_EXISTS) {
        DWORD attr = GetFileAttributesW(dir);
        if (attr != INVALID_FILE_ATTRIBUTES && (attr & FILE_ATTRIBUTE_DIRECTORY)) return;
    }
    if (code == ERROR_PATH_NOT_FOUND) {
        wchar_t *parent = _wcsdup(dir);
        if (!parent) fail("out of memory");
        wchar_t *slash = wcsrchr(parent, L'/');
        if (slash && slash > parent + 2) {
            *slash = 0;
            mkdirs(parent);
            free(parent);
            if (CreateDirectoryW(dir, NULL)) return;
        } else free(parent);
    }
    fail("cannot create container directory");
}

/* Quote a configuration path, independently of Windows command-line quoting.
 * NGINX compiles variables in root and certificate paths: reject '$' instead
 * of silently interpreting a literal filesystem name as a variable.
 */
static char *quoted(const wchar_t *value)
{
    int n = WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS, value, -1, NULL, 0, NULL, NULL);
    if (!n) fail("cannot encode path as UTF-8");
    char *utf8 = alloc((size_t)n), *out = alloc((size_t)n * 2 + 3), *p = out;
    if (!WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS, value, -1, utf8, n, NULL, NULL)) fail("cannot encode path");
    *p++ = '"';
    for (const char *s = utf8; *s; s++) {
        if ((unsigned char)*s < 32 || *s == '$') fail("configuration paths cannot contain control characters or '$'");
        if (*s == '"') *p++ = '\\';
        *p++ = *s == '\\' ? '/' : *s;
    }
    *p++ = '"';
    *p = 0;
    free(utf8);
    return out;
}

static char *qpath(const wchar_t *dir, const wchar_t *name)
{
    wchar_t *path = join(dir, name);
    char *out = quoted(path);
    free(path);
    return out;
}

static char *port(const wchar_t *name)
{
    const wchar_t *value = env(name);
    if (!value) fail("missing NGINX port environment variable");
    unsigned int n = 0;
    for (const wchar_t *p = value; *p; p++) {
        if (*p < L'0' || *p > L'9' || n > 6553) fail("invalid NGINX port");
        n = n * 10 + (unsigned int)(*p - L'0');
    }
    if (!n || n > 65535) fail("invalid NGINX port");
    char *out = alloc(6);
    snprintf(out, 6, "%u", n);
    return out;
}

static char *readfile(const wchar_t *path)
{
    FILE *f = _wfopen(path, L"rb");
    if (!f) fail("cannot open configuration template");
    if (fseek(f, 0, SEEK_END)) fail("cannot read configuration template");
    long n = ftell(f);
    if (n < 0 || n > 1024 * 1024 || fseek(f, 0, SEEK_SET)) fail("invalid configuration template");
    char *out = alloc((size_t)n + 1);
    if (fread(out, 1, (size_t)n, f) != (size_t)n || fclose(f)) fail("cannot read configuration template");
    return out;
}

static void generate(const wchar_t *app, const wchar_t *data, const wchar_t *config)
{
    const wchar_t *root = env(L"NGINX_DOCUMENT_ROOT");
    const wchar_t *cert = env(L"NGINX_TLS_CERT_FILE"), *key = env(L"NGINX_TLS_KEY_FILE");
    if (!!cert != !!key) fail("NGINX_TLS_CERT_FILE and NGINX_TLS_KEY_FILE must be supplied together");
    char *tls = alloc(CAP * 4);
    if (cert) {
        char *c = quoted(absolute(cert)), *k = quoted(absolute(key)), *p = port(L"NGINX_HTTPS_PORT");
        snprintf(tls, CAP * 4, "listen 127.0.0.1:%s ssl;\n        http2 on;\n        ssl_protocols TLSv1.2 TLSv1.3;\n        ssl_certificate %s;\n        ssl_certificate_key %s;", p, c, k);
        free(c); free(k); free(p);
    }
    struct { const char *token; char *value; } tokens[] = {
        {"@PID@", qpath(data, L"logs/nginx.pid")},
        {"@ERROR@", qpath(data, L"logs/error.log")},
        {"@MIME@", qpath(app, L"conf/mime.types")},
        {"@ACCESS@", qpath(data, L"logs/access.log")},
        {"@CLIENT@", qpath(data, L"temp/client_body")},
        {"@PROXY@", qpath(data, L"temp/proxy")},
        {"@FASTCGI@", qpath(data, L"temp/fastcgi")},
        {"@UWSGI@", qpath(data, L"temp/uwsgi")},
        {"@SCGI@", qpath(data, L"temp/scgi")},
        {"@HTTP@", port(L"NGINX_HTTP_PORT")},
        {"@ROOT@", root ? quoted(absolute(root)) : qpath(app, L"html")},
        {"@TLS@", tls},
        {"@SNIPPETS@", qpath(data, L"conf.d/*.conf")}
    };
    wchar_t suffix[64];
    swprintf(suffix, 64, L"conf/nginx.%lu.tmp", GetCurrentProcessId());
    wchar_t *tmp = join(data, suffix);
    FILE *f = _wfopen(tmp, L"wb");
    if (!f) fail("cannot create generated configuration");
    char *text = readfile(join(app, L"conf/nginx.conf.in"));
    for (char *p = text; *p;) {
        size_t i;
        for (i = 0; i < sizeof(tokens) / sizeof(tokens[0]); i++) {
            size_t len = strlen(tokens[i].token);
            if (!strncmp(p, tokens[i].token, len)) {
                if (fputs(tokens[i].value, f) == EOF) fail("cannot write configuration");
                p += len;
                break;
            }
        }
        if (i == sizeof(tokens) / sizeof(tokens[0]) && fputc(*p++, f) == EOF) fail("cannot write configuration");
    }
    if (fclose(f)) fail("cannot save generated configuration");
    if (!MoveFileExW(tmp, config, MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
        DeleteFileW(tmp);
        fail("cannot replace generated configuration");
    }
    for (size_t i = 0; i < sizeof(tokens) / sizeof(tokens[0]); i++) free(tokens[i].value);
    free(tmp); free(text);
}

static void append(wchar_t ch)
{
    if (used + 1 >= CAP) fail("command line is too long");
    command[used++] = ch;
    command[used] = 0;
}

static void argument(const wchar_t *value)
{
    if (used) append(L' ');
    append(L'"');
    while (*value) {
        size_t slashes = 0;
        while (*value == L'\\') { slashes++; value++; }
        size_t count = (*value == L'"' || !*value) ? 2 * slashes : slashes;
        while (count--) append(L'\\');
        if (!*value) break;
        if (*value == L'"') append(L'\\');
        append(*value++);
    }
    append(L'"');
}

static BOOL WINAPI control(DWORD event)
{
    /* NGINX receives console events too. Keep waiting for its shutdown. */
    return event == CTRL_C_EVENT || event == CTRL_BREAK_EVENT;
}

int wmain(int argc, wchar_t **argv)
{
    wchar_t module[CAP];
    DWORD n = GetModuleFileNameW(NULL, module, CAP);
    if (!n || n >= CAP) fail("cannot locate the image");
    wchar_t *slash = wcsrchr(module, L'\\');
    if (!slash) fail("invalid launcher path");
    *slash = 0;
    slash = wcsrchr(module, L'\\');
    if (!slash) fail("invalid image path");
    *slash = 0;
    wchar_t *app = absolute(module), *exe = join(app, L"libexec/nginx.exe");
    int custom = 0, prefix = 0, errorlog = 0, signal = 0, info = 0, test = 0;
    for (int i = 1; i < argc; i++) {
        if (argv[i][0] != L'-') continue;
        for (size_t j = 1; argv[i][j]; j++) {
            wchar_t ch = argv[i][j];
            if (ch == L'v' || ch == L'V' || ch == L'h' || ch == L'?') info = 1;
            if (ch == L't' || ch == L'T') test = 1;
            if (wcschr(L"cpegs", ch)) {
                if (ch == L'c') custom = 1;
                if (ch == L'p') prefix = 1;
                if (ch == L'e') errorlog = 1;
                if (ch == L's') signal = 1;
                if (!argv[i][j + 1] && ++i >= argc) fail("option requires an argument");
                break;
            }
        }
    }
    argument(exe);
    if (!info || test) {
        const wchar_t *value = env(L"NGINX_DATA");
        if (!value) fail("NGINX_DATA must name a directory");
        wchar_t *data = absolute(value), *config = join(data, L"conf/nginx.conf");
        if (!signal) {
            const wchar_t *dirs[] = {L"conf", L"conf.d", L"logs", L"temp", L"temp/client_body", L"temp/proxy", L"temp/fastcgi", L"temp/uwsgi", L"temp/scgi"};
            for (size_t i = 0; i < sizeof(dirs) / sizeof(dirs[0]); i++) mkdirs(join(data, dirs[i]));
            if (!custom) generate(app, data, config);
        }
        if (!prefix) {
            argument(L"-p");
            argument(join(data, L""));
        }
        if (!custom) { argument(L"-c"); argument(config); }
        if (!errorlog) { argument(L"-e"); argument(join(data, L"logs/error.log")); }
        free(data); free(config);
    }
    for (int i = 1; i < argc; i++) argument(argv[i]);
    STARTUPINFOW startup = {.cb = sizeof(startup)};
    PROCESS_INFORMATION process;
    startup.dwFlags = STARTF_USESTDHANDLES;
    startup.hStdInput = GetStdHandle(STD_INPUT_HANDLE);
    startup.hStdOutput = GetStdHandle(STD_OUTPUT_HANDLE);
    startup.hStdError = GetStdHandle(STD_ERROR_HANDLE);
    SetConsoleCtrlHandler(control, TRUE);
    if (!CreateProcessW(exe, command, NULL, NULL, TRUE, 0, NULL, NULL, &startup, &process)) fail("cannot start NGINX");
    CloseHandle(process.hThread);
    DWORD result = 1;
    if (WaitForSingleObject(process.hProcess, INFINITE) != WAIT_OBJECT_0 || !GetExitCodeProcess(process.hProcess, &result)) fail("cannot wait for NGINX");
    CloseHandle(process.hProcess);
    free(app); free(exe);
    return (int)result;
}
