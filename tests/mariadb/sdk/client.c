#include <mysql.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#define WIN32_LEAN_AND_MEAN
#include <windows.h>

int main(int argc, char **argv)
{
    if (argc == 3 && !strcmp(argv[1], "--copy-plugin")) {
        char module[32768];
        DWORD length = GetModuleFileNameA(NULL, module, sizeof(module));
        if (!length || length >= sizeof(module)) return 1;
        char *slash = strrchr(module, '\\');
        if (!slash) return 1;
        *slash = 0;
        slash = strrchr(module, '\\');
        if (!slash || (size_t)(slash - module) + 26 >= sizeof(module)) return 1;
        strcpy(slash, "\\lib\\plugin\\qd_probe.dll");
        return CopyFileA(module, argv[2], FALSE) ? 0 : 1;
    }
    const char *home = getenv("MARIADB_HOME");
    const char *port = getenv("MYSQL_TCP_PORT");
    char file[32768];
    if (!home || snprintf(file, sizeof(file), "%s/my.ini", home) >= (int)sizeof(file)) return 1;
    MYSQL *client = mysql_init(NULL);
    if (!client) return 1;
    unsigned timeout = 3;
    mysql_options(client, MYSQL_OPT_CONNECT_TIMEOUT, &timeout);
    mysql_options(client, MYSQL_READ_DEFAULT_FILE, file);
    mysql_options(client, MYSQL_READ_DEFAULT_GROUP, "client");
    char certificate[32768];
    if (snprintf(certificate, sizeof(certificate), "%s/server.crt", home) >= (int)sizeof(certificate)) {
        mysql_close(client);
        return 1;
    }
    my_bool verify = 1;
    mysql_options(client, MYSQL_OPT_SSL_CA, certificate);
    mysql_options(client, MYSQL_OPT_SSL_ENFORCE, &verify);
    mysql_options(client, MYSQL_OPT_SSL_VERIFY_SERVER_CERT, &verify);
    if (!mysql_real_connect(client, "127.0.0.1", NULL, NULL, NULL,
                            port ? (unsigned)atoi(port) : 3306, NULL, 0) ||
        mysql_query(client, "SELECT 40 + 2")) {
        fprintf(stderr, "C client: %s\n", mysql_error(client));
        mysql_close(client);
        return 1;
    }
    MYSQL_RES *result = mysql_store_result(client);
    MYSQL_ROW row = result ? mysql_fetch_row(result) : NULL;
    int success = row && row[0] && !strcmp(row[0], "42");
    if (success) printf("Connector/C %s: 42\n", mysql_get_client_info());
    if (result) mysql_free_result(result);
    mysql_close(client);
    return success ? 0 : 1;
}

