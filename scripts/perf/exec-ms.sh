#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Использование: exec-ms.sh [--bin ПУТЬ] <подкоманда> <вход.json>" >&2
  exit 2
}

bin="${HOME}/bin/chooks"
while (($#)); do
  case "$1" in
    --bin)
      (($# >= 2)) || usage
      bin=$2
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
[[ -x $bin ]] || { echo "Бинарь не найден или не исполняемый: $bin" >&2; exit 2; }
[[ -f $input ]] || { echo "Входной файл не найден: $input" >&2; exit 2; }
command -v timeit >/dev/null || { echo "Нет timeit в PATH (akit)" >&2; exit 2; }

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
transcript="$script_dir/testdata/transcript.jsonl"
[[ -f $transcript ]] || { echo "Транскрипт не найден: $transcript" >&2; exit 2; }
# @REPO@ во входе — корень этого репо: statusline и хуки видят живое git-дерево
repo=$(cd -- "$script_dir/../.." && pwd)
unset KONSOLE_DBUS_SERVICE KONSOLE_DBUS_SESSION KONSOLE_DBUS_WINDOW CLAUDE_PID

# Один каталог на все прогоны: сетап внутри команды timeit вошёл бы в замер
run_dir=$(mktemp -d -t chooks-perf.XXXXXX)
trap 'rm -rf -- "$run_dir"' EXIT
mkdir -p "$run_dir/state" "$run_dir/config" "$run_dir/runtime"
awk -v transcript="$transcript" -v repo="$repo" '{ gsub(/@TRANSCRIPT@/, transcript); gsub(/@REPO@/, repo); print }' "$input" >"$run_dir/input.json"

# Прогоны и бюджет — умолчания timeit, они же из FORMAT.md ночного обхода perf:
# до 5 прогонов, 45 с, лимит 60 с. Любой сбой — код 2: код 3 у ночи значит «мало данных»
if ! HOME="$run_dir" XDG_STATE_HOME="$run_dir/state" XDG_CONFIG_HOME="$run_dir/config" XDG_RUNTIME_DIR="$run_dir/runtime" \
  timeit --stdin "$run_dir/input.json" -- "$bin" "$subcommand"; then
  echo "Ошибка запуска подкоманды: $subcommand" >&2
  exit 2
fi
