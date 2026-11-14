#!/usr/bin/env bash
# Helpers for repository scripts. App/tool suites explicitly choose their image's
# default exported command as an entrypoint; CLI integration tests exercise CMD
# replacement directly. All metadata comes from inspect, never the store layout.

QD_IMAGES_HELPER="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/images.sh"
export QD_IMAGES_HELPER

qd_manifest() { local out; out=$(QUASIDOCK_HOME="${QUASIDOCK_HOME:-}" "$1" inspect -f '{{.Id}}' "$2") || return; printf '%s\n' "${out//$'\r'/}"; }
qd_version() { local out; out=$(QUASIDOCK_HOME="${QUASIDOCK_HOME:-}" "$1" inspect -f '{{.Quasidock.Version}}' "$2") || return; printf '%s\n' "${out//$'\r'/}"; }
qd_source() { local out; out=$(QUASIDOCK_HOME="${QUASIDOCK_HOME:-}" "$1" inspect -f '{{.Source}}' "$2") || return; printf '%s\n' "${out//$'\r'/}"; }
qd_cached() { QUASIDOCK_HOME="${QUASIDOCK_HOME:-}" "$1" inspect "$2" >/dev/null 2>&1; }

qd_run() {
    local cli=$1
    shift
    local -a options=()
    local explicit=false
    while [ $# -gt 0 ]; do
        case $1 in
            --entrypoint=*) explicit=true; options+=("$1"); shift ;;
            --entrypoint) explicit=true; options+=("$1" "$2"); shift 2 ;;
            --verbose|--ephemeral) options+=("$1"); shift ;;
            --*=*) options+=("$1"); shift ;;
            --) options+=("$1"); shift; break ;;
            --*|-e) options+=("$1" "$2"); shift 2 ;;
            *) break ;;
        esac
    done
    if [ $# -eq 0 ] || $explicit; then
        if [ "${QD_EXEC_RUN:-0}" = 1 ]; then exec "$cli" run "${options[@]}" "$@"
        else "$cli" run "${options[@]}" "$@"; fi
        return
    fi
    local image=$1 entrypoint
    # Prefer the export matching the default command and arguments: its embedded
    # placeholders are expanded by the runtime, including Java/Python launchers.
    entrypoint=$("$cli" inspect -f '{{$c := .Quasidock.Command}}{{$a := json .Quasidock.Args}}{{range $n, $e := .Quasidock.Exports}}{{if and (eq $e.Command $c) (eq (json $e.Args) $a)}}{{$n}}{{"\n"}}{{end}}{{end}}' "$image" 2>/dev/null | tr -d '\r' | head -1)
    if [ -z "$entrypoint" ]; then entrypoint=$("$cli" inspect -f '{{.Quasidock.Command}}' "$image" 2>/dev/null | tr -d '\r'); fi
    if [ "${QD_EXEC_RUN:-0}" = 1 ]; then
        if [ -n "$entrypoint" ]; then exec "$cli" run --entrypoint="$entrypoint" "${options[@]}" "$@"
        else exec "$cli" run "${options[@]}" "$@"; fi
    elif [ -n "$entrypoint" ]; then "$cli" run --entrypoint="$entrypoint" "${options[@]}" "$@"
    else "$cli" run "${options[@]}" "$@"; fi
}

qd_push() {
    local cli=$1
    shift
    local -a options=()
    if [ "$1" = --verbose ]; then options+=(--verbose); shift; fi
    local source=$1 target=$2
    case ${target##*/} in *:*|*@*) ;; *) target="$target:$(qd_version "$cli" "$source")" ;; esac
    "$cli" tag "$source" "$target" && "$cli" "${options[@]}" push "$target"
}

qd_remove_app() {
    local cli=$1 app=$2 digest id
    while IFS= read -r digest; do
        [ -n "$digest" ] || continue
        id=$("$cli" inspect -f '{{.Quasidock.ID}}' "$digest" 2>/dev/null | tr -d '\r') || return
        [ "${id,,}" != "${app,,}" ] || "$cli" rmi "$digest" || return
    done < <("$cli" images -q | tr -d '\r')
}

export -f qd_manifest qd_version qd_source qd_cached qd_run qd_push qd_remove_app

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    action=$1
    shift
    case $action in
        run) QD_EXEC_RUN=1 qd_run "$@" ;;
        remove) qd_remove_app "$@" ;;
        push) qd_push "$@" ;;
        *) echo "usage: images.sh <run|remove|push> QUASIDOCK [args...]" >&2; exit 2 ;;
    esac
fi
