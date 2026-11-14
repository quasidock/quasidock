/* Compiled into the SDK executables; paths are relative to the deployed SDK. */
static const struct command commands[] = {
    { L"gradle.exe", L"%QUASIDOCK_GRADLE_JAVA%", { L"-Xmx64m", L"-Xms64m", L"-javaagent:%QUASIDOCK_SDK_DIR%\\lib\\agents\\gradle-instrumentation-agent-9.8.1.jar", L"-Dorg.gradle.appname=gradle", L"-Dorg.gradle.daemon=false", L"-jar", L"%QUASIDOCK_SDK_DIR%\\lib\\gradle-gradle-cli-main-9.8.1.jar" }, NULL },
    { L"gradle-vendor.exe", L"%QUASIDOCK_GRADLE_JAVA%", { L"%QUASIDOCK_SDK_DIR%\\zigsaw\\GradleVendor.java" }, NULL },
};
