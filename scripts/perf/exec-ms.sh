#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Использование: exec-ms.sh [--runs N] [--bin ПУТЬ] <подкоманда> <вход.json>" >&2
  exit 2
}

runs=11
bin="${HOME}/bin/chooks"
while (($#)); do
  case "$1" in
    --runs|--bin)
      (($# >= 2)) || usage
      if [[ $1 == --runs ]]; then runs=$2; else bin=$2; fi
      shift 2
      ;;
    --) shift; break ;;
    -*) usage ;;
    *) break ;;
  esac
done
[[ $# == 2 ]] || usage
subcommand=$1
input=$2
[[ $runs =~ ^[1-9][0-9]*$ ]] || usage
[[ -x $bin ]] || { echo "Бинарь не найден или не исполняемый: $bin" >&2; exit 2; }
[[ -f $input ]] || { echo "Входной файл не найден: $input" >&2; exit 2; }

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
transcript="$script_dir/testdata/transcript.jsonl"
[[ -f $transcript ]] || { echo "Транскрипт не найден: $transcript" >&2; exit 2; }
input=$(cd -- "$(dirname -- "$input")" && printf '%s/%s\n' "$PWD" "$(basename -- "$input")")
unset KONSOLE_DBUS_SERVICE KONSOLE_DBUS_SESSION KONSOLE_DBUS_WINDOW CLAUDE_PID

tmpdirs=()
cleanup() {
  local dir
  for dir in "${tmpdirs[@]}"; do rm -rf -- "$dir"; done
}
trap cleanup EXIT
times=$(mktemp -t chooks-perf-times.XXXXXX)
trap 'cleanup; rm -f -- "$times"' EXIT

for ((i = 0; i < runs; i++)); do
  run_dir=$(mktemp -d -t chooks-perf.XXXXXX)
  tmpdirs+=("$run_dir")
  mkdir -p "$run_dir/state" "$run_dir/config" "$run_dir/runtime"
  awk -v transcript="$transcript" '{ gsub(/@TRANSCRIPT@/, transcript); print }' "$input" >"$run_dir/input.json"
  start=$(date +%s%N)
  if ! HOME="$run_dir" XDG_STATE_HOME="$run_dir/state" XDG_CONFIG_HOME="$run_dir/config" XDG_RUNTIME_DIR="$run_dir/runtime" \
    "$bin" "$subcommand" <"$run_dir/input.json" >/dev/null 2>"$run_dir/stderr"; then
    cat "$run_dir/stderr" >&2
    echo "Ошибка запуска подкоманды: $subcommand" >&2
    exit 2
  fi
  end=$(date +%s%N)
  awk -v start="$start" -v end="$end" 'BEGIN { printf "%.3f\n", (end-start)/1000000 }' >>"$times"
done
sort -n "$times" | awk '{ a[NR]=$1 } END { if (NR % 2) printf "%.1f\n", a[(NR+1)/2]; else printf "%.1f\n", (a[NR/2]+a[NR/2+1])/2 }'
