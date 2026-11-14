/* Compiled into the SDK executables; paths are relative to the deployed SDK. */
static const struct command commands[] = {
    { L"meson.exe", L"%QUASIDOCK_SDK_DIR%\\python.exe", { L"%QUASIDOCK_SDK_DIR%\\meson\\meson.py" }, NULL },
    { L"ninja.exe", L"%QUASIDOCK_SDK_DIR%\\ninja.exe", { NULL }, NULL },
    { L"pkg-config.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\pkgconf.exe", { NULL }, NULL },
    { L"pkgconf.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\pkgconf.exe", { NULL }, NULL },
};
