cmake_minimum_required(VERSION 3.25)
project(quasidock_mariadb_probe LANGUAGES C CXX)

find_package(PkgConfig REQUIRED)
pkg_check_modules(MARIADB REQUIRED IMPORTED_TARGET libmariadb)
add_executable(qd-client client.c)
target_link_libraries(qd-client PRIVATE PkgConfig::MARIADB)
install(TARGETS qd-client RUNTIME DESTINATION bin)

# Check the static client archive and pkg-config's transitive dependencies too.
find_library(MARIADB_STATIC_ARCHIVE NAMES mariadbclient
    HINTS ${MARIADB_LIBRARY_DIRS} NO_DEFAULT_PATH REQUIRED)
add_executable(qd-client-static client.c)
target_include_directories(qd-client-static PRIVATE ${MARIADB_INCLUDE_DIRS})
target_link_directories(qd-client-static PRIVATE ${MARIADB_STATIC_LIBRARY_DIRS})
set(static_dependencies ${MARIADB_STATIC_LIBRARIES})
list(REMOVE_ITEM static_dependencies mariadb)
target_link_libraries(qd-client-static PRIVATE ${MARIADB_STATIC_ARCHIVE} ${static_dependencies})
install(TARGETS qd-client-static RUNTIME DESTINATION bin)

find_package(mariadb-plugin CONFIG REQUIRED)
mariadb_add_plugin(qd_probe probe.cc MODULE_ONLY
    AUTHOR Quasidock DESCRIPTION "Installed SDK integration test" VERSION 1.0)
# Use a server symbol as well as the stable plugin services API. This checks
# the installed import library, not just a self-contained DLL's registration.
target_link_libraries(qd_probe mariadb_private server)

