#include <my_global.h>
#include <my_sys.h>
#include <mysql/plugin.h>
#include <mysql/service_my_print_error.h>

static long long started;
static int initialize(void *)
{
    started = (long long)my_hrtime().val;
    my_printf_error(0, "Quasidock SDK plugin initialized", ME_NOTE | ME_ERROR_LOG_ONLY);
    return started ? 0 : 1;
}

static struct st_mysql_daemon daemon = { MYSQL_DAEMON_INTERFACE_VERSION };
static struct st_mysql_show_var status[] = {
    { "started", (char *)&started, SHOW_LONGLONG },
    { NULL, NULL, SHOW_UNDEF }
};

maria_declare_plugin(qd_probe)
{
    MYSQL_DAEMON_PLUGIN,
    &daemon,
    "qd_probe",
    "Quasidock",
    "Installed SDK integration test",
    PLUGIN_LICENSE_GPL,
    initialize,
    NULL,
    0x0100,
    status,
    NULL,
    "1.0",
    MariaDB_PLUGIN_MATURITY_STABLE
}
maria_declare_plugin_end;

