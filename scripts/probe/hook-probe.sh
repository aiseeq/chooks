#!/usr/bin/env bash
# Ручная проба подкоманды хука chooks в отдельном HOME: лог, конфиг и
# состояние — во временной папке. Живой ~/.claude/logs/chooks.log считает
# Сон (таймауты хуков), строки проб туда попадать не должны
set -euo pipefail

usage() {
  echo "Использование: hook-probe.sh [--bin ПУТЬ] [--dir ПАПКА] <подкоманда>  (вход хука — JSON на stdin)" >&2
  echo "  --dir — готовая папка пробы (например, с занятыми блокировками в run/chooks/sessions); без него — новая" >&2
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

real_home=$HOME
[[ -n $dir ]] || dir=$(mktemp -d -t chooks-probe.XXXXXX)
mkdir -p "$dir/.claude/logs" "$dir/.config/chooks" "$dir/.local/state" "$dir/run"

# Таймауты хуков для строки «hook slow» — из настоящего settings.json
if [[ -f $real_home/.claude/settings.json ]]; then
  cp "$real_home/.claude/settings.json" "$dir/.claude/settings.json"
fi
# Конфиг с путём лога от ~: при подменённом HOME лог уходит в папку пробы.
# Абсолютный путь вёл бы в живой лог — такой конфиг не копируется
config=$real_home/.config/chooks/config.yaml
if [[ -f $config ]]; then
  if grep -Eq '^[[:space:]]*file:[[:space:]]*"?/' "$config"; then
    echo "В $config абсолютный путь лога: проба писала бы в живой лог" >&2
    exit 2
  fi
  cp "$config" "$dir/.config/chooks/config.yaml"
fi

# Без окна Konsole и сессии Claude: проба не меняет заголовок настоящей
# вкладки и не путает свою сессию с живой (как scripts/perf/exec-ms.sh)
unset KONSOLE_DBUS_SERVICE KONSOLE_DBUS_SESSION KONSOLE_DBUS_WINDOW CLAUDE_PID CLAUDE_CONFIG_DIR

code=0
env HOME="$dir" XDG_CONFIG_HOME="$dir/.config" XDG_STATE_HOME="$dir/.local/state" \
  XDG_RUNTIME_DIR="$dir/run" "$bin" "$subcommand" || code=$?
echo "Папка пробы: $dir; лог: $dir/.claude/logs/chooks.log" >&2
exit "$code"
