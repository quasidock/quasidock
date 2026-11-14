/* Compiled into the SDK executables; paths are relative to the deployed SDK. */
static const struct command commands[] = {
    { L"cc.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\x86_64-w64-mingw32-clang.exe", { NULL }, NULL },
    { L"c++.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\x86_64-w64-mingw32-clang++.exe", { NULL }, NULL },
    { L"ar.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\llvm-ar.exe", { NULL }, NULL },
    { L"ranlib.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\llvm-ranlib.exe", { NULL }, NULL },
    { L"nm.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\llvm-nm.exe", { NULL }, NULL },
    { L"strip.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\llvm-strip.exe", { NULL }, NULL },
    { L"objdump.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\llvm-objdump.exe", { NULL }, NULL },
    { L"windres.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\x86_64-w64-mingw32-windres.exe", { NULL }, NULL },
    { L"dlltool.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\x86_64-w64-mingw32-dlltool.exe", { NULL }, NULL },
    { L"as.exe", L"%QUASIDOCK_SDK_DIR%\\bin\\x86_64-w64-mingw32-clang.exe", { L"-c", L"-x", L"assembler" }, L"--64" },
};
