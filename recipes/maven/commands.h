/* Compiled into the SDK executables; paths are relative to the deployed SDK. */
static const struct command commands[] = {
    { L"mvn.exe", L"%QUASIDOCK_MAVEN_JAVA%", { L"--enable-native-access=ALL-UNNAMED", L"-classpath", L"%QUASIDOCK_SDK_DIR%\\boot\\plexus-classworlds-2.12.1.jar", L"-Dclassworlds.conf=%QUASIDOCK_SDK_DIR%\\bin\\m2.conf", L"-Dmaven.home=%QUASIDOCK_SDK_DIR%", L"-Dlibrary.jline.path=%QUASIDOCK_SDK_DIR%\\lib\\jline-native", L"-Dorg.jline.terminal.exec.redirectPipeCreationMode=native,reflection", L"-Dmaven.multiModuleProjectDirectory=.", L"org.codehaus.plexus.classworlds.launcher.Launcher" }, NULL },
};
