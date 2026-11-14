#!/usr/bin/env bash
# Helpers for repository scripts. App/tool suites preserve the image's
# entrypoint when passing arguments; CLI integration tests exercise command
# replacement directly. All metadata comes from inspect, never the store layout.

QD_IMAGES_HELPER="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/images.sh"
export QD_IMAGES_HELPER

qd_manifest() { local out; out=$(QUASIDOCK_HOME="${QUASIDOCK_HOME:-}" "$1" image inspect -f '{{.Id}}' "$2") || return; printf '%s\n' "${out//$'\r'/}"; }
qd_version() { local out; out=$(QUASIDOCK_HOME="${QUASIDOCK_HOME:-}" "$1" image inspect -f '{{.Quasidock.Version}}' "$2") || return; printf '%s\n' "${out//$'\r'/}"; }
qd_source() { local out; out=$(QUASIDOCK_HOME="${QUASIDOCK_HOME:-}" "$1" image inspect -f '{{.Source}}' "$2") || return; printf '%s\n' "${out//$'\r'/}"; }
qd_cached() { QUASIDOCK_HOME="${QUASIDOCK_HOME:-}" "$1" image inspect "$2" >/dev/null 2>&1; }
qd_container_data() { local out; out=$(QUASIDOCK_HOME="${QUASIDOCK_HOME:-}" "$1" container inspect -f '{{.DataDir}}' "$2") || return; printf '%s\n' "${out//$'\r'/}"; }

qd_run() {
    local cli=$1
    shift
    local -a options=()
    local explicit=false
    while [ $# -gt 0 ]; do
        case $1 in
            --entrypoint=*) explicit=true; options+=("$1"); shift ;;
            --entrypoint) explicit=true; options+=("$1" "$2"); shift 2 ;;
            --verbose|--rm|-d|--detach|-i|--interactive|-t|--tty|-it|-ti|-dit|-di|-dt|-id|-td|-itd) options+=("$1"); shift ;;
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
    local image=$1 entrypoint=
    # Preserve the image's entrypoint when trailing arguments are passed: multi-element
    # entrypoints select the image ID so placeholders and leading args stay intact.
    if [ $# -gt 1 ]; then
        entrypoint=$("$cli" image inspect -f '{{if gt (len .Quasidock.Entrypoint) 1}}{{.Quasidock.ID}}{{else if .Quasidock.Entrypoint}}{{index .Quasidock.Entrypoint 0}}{{end}}' "$image" 2>/dev/null | tr -d '\r')
    fi
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
    local cli=$1 app=$2 cid info id running digest
    while IFS= read -r cid; do
        [ -n "$cid" ] || continue
        info=$("$cli" container inspect -f '{{.AppID}} {{.State.Running}}' "$cid" 2>/dev/null) || continue
        info=${info//$'\r'/}
        id=${info%% *}
        running=${info#* }
        if [ "${id,,}" = "${app,,}" ] && [ "$running" = "false" ]; then
            "$cli" rm "$cid" >/dev/null 2>&1 || true
        fi
    done < <("$cli" ps -aq 2>/dev/null | tr -d '\r')
    while IFS= read -r digest; do
        [ -n "$digest" ] || continue
        id=$("$cli" image inspect -f '{{.Quasidock.ID}}' "$digest" 2>/dev/null) || return
        id=${id//$'\r'/}
        [ "${id,,}" != "${app,,}" ] || "$cli" rmi "$digest" || return
    done < <("$cli" images -q | tr -d '\r')
}

export -f qd_manifest qd_version qd_source qd_cached qd_container_data qd_run qd_push qd_remove_app

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
