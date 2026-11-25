/* Invoke OTP's unmodified erlexec.dll without GetLongPathNameW's ancestor
 * traversal, which is unavailable inside an AppContainer. The executable's
 * deployment path supplies the runtime root; no installation file is written. */
#include <windows.h>
#include <stdio.h>
#include <stdlib.h>
#include <wchar.h>

typedef int erlexec_function(int, char **, HANDLE);

int wmain(int argc, wchar_t **argv) {
    wchar_t root[32768], bindir[32768], dll[32768];
    DWORD n = GetModuleFileNameW(NULL, root, 32768);
    if (!n || n >= 32768) return 127;
    wchar_t *slash = wcsrchr(root, L'\\');
    if (!slash) return 127;
    *slash = 0; /* bin */
    slash = wcsrchr(root, L'\\');
    if (!slash) return 127;
    *slash = 0; /* root, or erts-version */
    slash = wcsrchr(root, L'\\');
    if (slash && !wcsncmp(slash + 1, L"erts-", 5)) *slash = 0;
    if (wcslen(root) + 30 >= 32768) return 127;
    swprintf(bindir, 32768, L"%ls\\erts-17.1.2\\bin", root);
    swprintf(dll, 32768, L"%ls\\erlexec.dll", bindir);
    if (!SetEnvironmentVariableW(L"ERLEXEC_DIR", bindir) || !SetDllDirectoryW(bindir)) return 127;
    DWORD length = GetEnvironmentVariableW(L"PATH", NULL, 0);
    wchar_t *old = calloc(length + 1, sizeof(wchar_t));
    wchar_t *path = calloc(length + wcslen(bindir) + 2, sizeof(wchar_t));
    if (!old || !path) return 127;
    GetEnvironmentVariableW(L"PATH", old, length + 1);
    swprintf(path, length + wcslen(bindir) + 2, L"%ls;%ls", bindir, old);
    if (!SetEnvironmentVariableW(L"PATH", path)) return 127;
    HMODULE module = LoadLibraryW(dll);
    if (!module) { fwprintf(stderr, L"erl: cannot load %ls (Windows error %lu)\n", dll, GetLastError()); return 127; }
    erlexec_function *start = (erlexec_function *)GetProcAddress(module, "win_erlexec");
    if (!start) return 127;
    char **args = calloc((size_t)argc + 1, sizeof(char *));
    if (!args) return 127;
    for (int i = 0; i < argc; i++) {
        int size = WideCharToMultiByte(CP_UTF8, 0, argv[i], -1, NULL, 0, NULL, NULL);
        if (!size || !(args[i] = malloc((size_t)size))) return 127;
        if (!WideCharToMultiByte(CP_UTF8, 0, argv[i], -1, args[i], size, NULL, NULL)) return 127;
    }
    return start(argc, args, module);
}
