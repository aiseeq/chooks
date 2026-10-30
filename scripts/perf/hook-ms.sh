#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Использование: hook-ms.sh [--q N] [--since ДЛИТ] [--until EPOCH] [--root КАТАЛОГ] <подкоманда>" >&2
  exit 2
}

q=95
since=24h
until_epoch=$(date +%s)
root="${HOME}/.claude/projects"
while (($#)); do
  case "$1" in
    --q|--since|--until|--root)
      (($# >= 2)) || usage
      key=$1
      value=$2
      shift 2
      case "$key" in
        --q) q=$value ;;
        --since) since=$value ;;
        --until) until_epoch=$value ;;
        --root) root=$value ;;
      esac
      ;;
    --) shift; break ;;
    -*) usage ;;
    *) break ;;
  esac
done
[[ $# == 1 ]] || usage
subcommand=$1
[[ $q =~ ^[1-9][0-9]?$|^100$ ]] || usage
[[ $until_epoch =~ ^[0-9]+$ ]] || usage
[[ $since =~ ^[0-9]+(s|m|h|d)$ ]] || usage
[[ -d $root ]] || { echo "Нет каталога транскриптов: $root" >&2; exit 2; }
command -v jq >/dev/null && command -v rg >/dev/null || { echo "Требуются jq и rg" >&2; exit 2; }

case ${since: -1} in
  s) factor=1 ;;
  m) factor=60 ;;
  h) factor=3600 ;;
  d) factor=86400 ;;
esac
since_epoch=$((until_epoch - ${since%?} * factor))
threshold=20
((q == 50)) && threshold=3

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
find "$root" -type f -name '*.jsonl' -newermt "@$since_epoch" -print0 >"$tmp/files"
if [[ ! -s $tmp/files ]]; then
  echo "Мало событий: найдено 0, нужно $threshold" >&2
  exit 3
fi

# rg без совпадений в части файлов даёт xargs код 123, поэтому сбой rg
# виден по его stderr
xargs -0 -r rg -F -e 'stop_hook_summary' -e '"hook_success"' --no-heading --no-filename <"$tmp/files" >"$tmp/lines" 2>"$tmp/rg.err" || true
if [[ -s $tmp/rg.err ]]; then
  cat "$tmp/rg.err" >&2
  exit 2
fi

# Записи обоих форматов; фильтр оставляет точное имя исполняемого файла и
# подкоманды. Недописанная последняя строка транскрипта, который пишется
# прямо сейчас, — не JSON: такие строки пропускаются
jq -R -r --arg sub "$subcommand" --argjson from "$since_epoch" --argjson until "$until_epoch" '
  def event($cmd; $duration; $ts):
    select(($cmd | type) == "string" and ($duration | type) == "number" and ($ts | type) == "string")
    | select($cmd | endswith(" " + $sub))
    | ($cmd | rtrimstr(" " + $sub) | split("/") | .[-1]) as $bin
    | select($bin == "chooks" or $bin == "claude-hooks")
    | ($ts | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) as $t
    | select($t > $from and $t <= $until)
    | $duration;
  fromjson? | . as $row |
  if ($row.type == "system" and $row.subtype == "stop_hook_summary") then
    $row.hookInfos[]? | event(.command; .durationMs; $row.timestamp)
  elif $row.attachment.type? == "hook_success" then
    event($row.attachment.command; $row.attachment.durationMs; $row.timestamp)
  else empty end
' "$tmp/lines" >"$tmp/durations"

count=0
[[ -f $tmp/durations ]] && count=$(wc -l <"$tmp/durations")
if ((count < threshold)); then
  echo "Мало событий: найдено $count, нужно $threshold" >&2
  exit 3
fi
rank=$(((q * count + 99) / 100))
sort -n "$tmp/durations" | sed -n "${rank}p" | awk '{printf "%d\n", $1}'
