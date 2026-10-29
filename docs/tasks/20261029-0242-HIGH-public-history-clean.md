# Вычистить имена рабочих проектов из истории публичного репо

Статус: в работе
Исполнитель: chooks-master

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать

## Приёмка
- `graft leaks <root>..main` и отдельно первый коммит: 0 терминов (кроме совпадений имён тестовых функций с функциями рабочих проектов).
- `git log --all -p | grep -iE` по именам рабочих проектов: пусто.
- Дерево нового main отличается от старого только заменёнными строками (`git diff --stat`).
- Гейт `graft smoke` зелёный.
- Итог - git-admin, копия graft-master.

## Задача
Вычистить из истории публичного chooks имена рабочих проектов (термины `~/.config/graft/private-terms`) и сделать force-push. Задание graft-master (Сон, ночь 29.10, через git-admin); решение пользователя - `~/.claude/decisions.md` public-repo-names (28.10 09:53 CET).
- `git clone --mirror` -> `git filter-repo --replace-text --replace-message` -> сверка деревьев -> `git ls-remote` (origin не сдвинулся) -> `git push --force` веток и тегов.
- В ~/git/chooks: `git fetch --force --tags`, `git reset --hard origin/main`; ветка dream/2026-10-28 - в archive/ или перенос; дерево Сна `~/.cache/dream-trees/2026-10-29/git-chooks` - `git switch --detach <новый>`, если дерево совпадает.
- Короткие хеши в docs/tasks пересчитать по `filter-repo/commit-map` обычным коммитом.
- Синтетический адрес кошелька из списка private-terms в первом коммите тоже заменить: иначе `graft leaks` на chooks всегда красный.


## Журнал
- 2026-10-29 02:42 открыт — chooks-master
- 2026-10-29 02:43 в работе — chooks-master
- 2026-10-29 02:43 note — chooks-master: mirror + filter-repo: main 3f38ae6 -> b7a094f, 156 коммитов, дерево отличается в 6 файлах только заменёнными строками; origin не сдвинулся; dream/2026-10-28 - содержимое в main (git cherry "-"); refs/pull/1..11 на GitHub содержат имя рабочего проекта - force-push их не перепишет. Делаю force-push main.
- 2026-10-29 02:45 note — chooks-master: force-push 3f38ae6 -> b7a094f сделан; ~/git/chooks reset на origin/main, ветка dream/2026-10-28 удалена (содержимое в main), дерево Сна 2026-10-29 переведено на b7a094f (было чистым). Хеши 9 старых коммитов в docs/tasks пересчитаны по commit-map. graft leaks root..HEAD: только совпадение имени тестовой функции с функцией рабочего проекта; первый коммит и git log --all -p по именам рабочих проектов - 0.
