#!/usr/bin/env bash
# Ручная проба подкоманды хука chooks в отдельном HOME: лог, конфиг и
# состояние — во временной папке. Живой ~/.claude/logs/chooks.log считает
# Сон (таймауты хуков), строки проб туда попадать не должны
set -euo pipefail

usage() {
  echo "Использование: hook-probe.sh [--bin ПУТЬ] [--dir ПАПКА] <подкоманда>  (вход хука — JSON на stdin)" >&2
  echo "  --dir — готовая папка пробы (например, с занятыми блокировками в run/chooks/sessions), остаётся после пробы;" >&2
  echo "          без него — новая временная, её лог печатается в stderr, папка удаляется" >&2
  exit 2
}

bin="${HOME}/bin/chooks"
dir=""
while (($#)); do
  case "$1" in
    --bin|--dir)
      (($# >= 2)) || usage
      [[ $1 == --bin ]] && bin=$2 || dir=$2
      shift 2
      ;;
    --) shift; break ;;
    -*) usage ;;
    *) break ;;
  esac
done
[[ $# == 1 ]] || usage
subcommand=$1
[[ -x $bin ]] || { echo "Нет исполняемого файла: $bin" >&2; exit 2; }
command -v yq >/dev/null || { echo "Требуется yq (mikefarah)" >&2; exit 2; }

real_home=$HOME
keep=1
if [[ -z $dir ]]; then
  dir=$(mktemp -d -t chooks-probe.XXXXXX)
  keep=0
fi
mkdir -p "$dir/.claude/logs" "$dir/.config/chooks" "$dir/.local/state" "$dir/run"

# Таймауты хуков для строки «hook slow» — из настоящего settings.json
if [[ -f $real_home/.claude/settings.json ]]; then
  cp "$real_home/.claude/settings.json" "$dir/.claude/settings.json"
fi
# Настройки хуков — из настоящего конфига, а лог, звук и уведомления
# задаются поверх, что бы там ни стояло: путь лога от ~ при подменённом HOME
# ведёт в папку пробы, а звонок пробы на рабочем столе не нужен никому
probe_config=$dir/.config/chooks/config.yaml
config=$real_home/.config/chooks/config.yaml
if [[ -f $config ]]; then
  cp "$config" "$probe_config"
else
  : >"$probe_config"
fi
yq -i '.logger.output = "file" | .logger.file = "~/.claude/logs/chooks.log"
  | .tools.notifier.sound = false | .tools.notifier.desktop = false' "$probe_config"

# Без окна Konsole, сессии Claude и сеансовой шины: проба не меняет
# заголовок настоящей вкладки, не путает свою сессию с живой
# (как scripts/perf/exec-ms.sh) и не достаёт до рабочего стола
unset KONSOLE_DBUS_SERVICE KONSOLE_DBUS_SESSION KONSOLE_DBUS_WINDOW CLAUDE_PID CLAUDE_CONFIG_DIR DBUS_SESSION_BUS_ADDRESS

# stderr хука — в файл: заголовок окна (OSC) из него не должен попасть в
# терминал человека, запустившего пробу
code=0
env HOME="$dir" XDG_CONFIG_HOME="$dir/.config" XDG_STATE_HOME="$dir/.local/state" \
  XDG_RUNTIME_DIR="$dir/run" "$bin" "$subcommand" 2>"$dir/stderr" || code=$?

log=$dir/.claude/logs/chooks.log
if ((keep)); then
  echo "Папка пробы: $dir; лог: $log; stderr хука: $dir/stderr; код выхода: $code" >&2
else
  echo "--- лог пробы ($log):" >&2
  [[ ! -f $log ]] || cat "$log" >&2
  echo "--- длительность:" >&2
  [[ ! -f $dir/.claude/logs/chooks-hooks.tsv ]] || cat "$dir/.claude/logs/chooks-hooks.tsv" >&2
  echo "--- код выхода: $code" >&2
  rm -rf -- "$dir"
fi
exit "$code"
