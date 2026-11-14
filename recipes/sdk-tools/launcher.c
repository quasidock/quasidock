/* Ordinary SDK executables, packaged by the recipes. Each SDK compiles its
 * commands.h into this program and installs copies under sdk-bin. Paths are
 * resolved from the deployed executable, never from a build-store location.
 * Quasidock itself does not interpret this table or create command aliases. */
#include <errno.h>
#include <process.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <wchar.h>
#include <windows.h>

struct command {
    const wchar_t *name;
    const wchar_t *exe;
    const wchar_t *args[16];
    const wchar_t *drop;
};

#include "commands.h"

/* _wspawnv joins its arguments with spaces. Quote them for the child's CRT,
 * including empty arguments, embedded quotes, and trailing backslashes. */
static wchar_t *quote(const wchar_t *arg) {
    size_t len = wcslen(arg);
    wchar_t *out = malloc((2 * len + 3) * sizeof *out), *o = out;
    if (!out) exit(1);
    *o++ = L'"';
    for (const wchar_t *p = arg;; p++) {
        size_t slashes = 0;
        while (*p == L'\\') { p++; slashes++; }
        if (!*p) {
            for (size_t i = 0; i < 2 * slashes; i++) *o++ = L'\\';
            break;
        }
        if (*p == L'"') slashes = 2 * slashes + 1;
        for (size_t i = 0; i < slashes; i++) *o++ = L'\\';
        *o++ = *p;
    }
    *o++ = L'"';
    *o = 0;
    return out;
}

static wchar_t *expand(const wchar_t *s) {
    for (const wchar_t *p = s; (p = wcschr(p, L'%')) != NULL;) {
        const wchar_t *end = wcschr(++p, L'%');
        if (!end || end - p >= 128) exit(127);
        wchar_t name[128];
        wmemcpy(name, p, (size_t)(end - p));
        name[end - p] = 0;
        if (!GetEnvironmentVariableW(name, NULL, 0)) {
            fwprintf(stderr, L"SDK command has an unset path variable: %ls\n", name);
            exit(127);
        }
        p = end + 1;
    }
    DWORD n = ExpandEnvironmentStringsW(s, NULL, 0);
    wchar_t *out = n ? calloc(n, sizeof *out) : NULL;
    if (!out) exit(1);
    if (!ExpandEnvironmentStringsW(s, out, n)) exit(127);
    return out;
}

static BOOL WINAPI control(DWORD event) {
    /* The child receives console events too; let it finish and return its
     * status rather than ending this wrapper before the child has exited. */
    return event == CTRL_C_EVENT || event == CTRL_BREAK_EVENT;
}

int wmain(int argc, wchar_t **argv) {
    wchar_t self[32768];
    DWORD n = GetModuleFileNameW(NULL, self, sizeof self / sizeof *self);
    if (!n || n >= sizeof self / sizeof *self) return 127;
    wchar_t *slash = wcsrchr(self, L'\\');
    if (!slash) return 127;
    const struct command *command = NULL;
    for (size_t i = 0; i < sizeof commands / sizeof *commands; i++) {
        if (!_wcsicmp(slash + 1, commands[i].name)) {
            command = &commands[i];
            break;
        }
    }
    if (!command) {
        fwprintf(stderr, L"unknown SDK command: %ls\n", slash + 1);
        return 127;
    }
    *slash = 0; /* sdk-bin */
    slash = wcsrchr(self, L'\\');
    if (!slash) return 127;
    *slash = 0; /* deployed SDK root */
    if (!SetEnvironmentVariableW(L"QUASIDOCK_SDK_DIR", self)) return 127;

    wchar_t *exe = expand(command->exe);
    const wchar_t **args = calloc((size_t)argc + 18, sizeof *args);
    if (!args) return 1;
    size_t count = 0;
    args[count++] = quote(exe);
    for (size_t i = 0; i < 16 && command->args[i]; i++) {
        wchar_t *arg = expand(command->args[i]);
        args[count++] = quote(arg);
        free(arg);
    }
    for (int i = 1; i < argc; i++) {
        if (!command->drop || wcscmp(command->drop, argv[i]))
            args[count++] = quote(argv[i]);
    }
    args[count] = NULL;
    SetConsoleCtrlHandler(control, TRUE);
    fflush(NULL);
    intptr_t status = _wspawnv(_P_WAIT, exe, args);
    if (status == -1) {
        fwprintf(stderr, L"can't run %ls: %hs\n", exe, strerror(errno));
        return 127;
    }
    return (int)status;
}
