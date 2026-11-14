/* Compiled into the SDK executables; paths are relative to the deployed SDK. */
static const struct command commands[] = {
    { L"cc.exe", L"%QUASIDOCK_SDK_DIR%\\zig.exe", { L"cc", L"-target", L"x86_64-windows-gnu" }, NULL },
    { L"c++.exe", L"%QUASIDOCK_SDK_DIR%\\zig.exe", { L"c++", L"-target", L"x86_64-windows-gnu" }, NULL },
    { L"ar.exe", L"%QUASIDOCK_SDK_DIR%\\zig.exe", { L"ar" }, NULL },
    { L"ranlib.exe", L"%QUASIDOCK_SDK_DIR%\\zig.exe", { L"ranlib" }, NULL },
    { L"rc.exe", L"%QUASIDOCK_SDK_DIR%\\zig.exe", { L"rc" }, NULL },
    { L"as.exe", L"%QUASIDOCK_SDK_DIR%\\zig.exe", { L"cc", L"-target", L"x86_64-windows-gnu", L"-c", L"-x", L"assembler" }, L"--64" },
};
