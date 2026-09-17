# bleve-spike - локальный bleve-индекс поверх истории OpenCode

Отдельный Go-модуль (свой `go.mod`) внутри репозитория opencode-rag, в каталоге
`bleve-spike/`. Основной код лежит уровнем выше (`../cmd`, `../internal`); это
вложенный модуль, поэтому `go build ./...` из корня его не собирает. Цель:
понять, как устроить локальный поисковый индекс на bleve, который, как и
PG-индекс в opencode-rag, возвращает координаты `(session_id, position)` и по
ним даёт читать оригинал.

Артефакты (`bin/`, `data/`) в git не идут - см. `bleve-spike/.gitignore`.

Ничего не пишет в `opencode.db`: SQLite открывается `mode=ro`.

## Быстрый старт

```bash
cd bleve-spike
go build -o bin/bleve-spike .

# 1. посмотреть схему/одну сессию
./bin/bleve-spike schema
./bin/bleve-spike stat --session ses_f7588cd6dffeW430jRLkmAHt6Y

# 2. собрать индекс по одной сессии
./bin/bleve-spike index --session ses_f7588cd6dffeW430jRLkmAHt6Y

# 3. искать: на выходе session_id + position + сниппет с подсветкой
./bin/bleve-spike search "swagger"
./bin/bleve-spike search "ASOCVulnerability" --type tool
./bin/bleve-spike search "матчинга" --limit 5
./bin/bleve-spike search "content:severity AND content:enum" --mode qs
./bin/bleve-spike search "swagger" --sort time

# 4. по координате из поиска прочитать оригинал (аналог memory_read)
./bin/bleve-spike read --session ses_f7588cd6dffeW430jRLkmAHt6Y --position 543 --before 2 --after 2

# 4b. один шаг: найти и сразу прочитать оригинал вокруг лучшего совпадения
./bin/bleve-spike ask "3 компонента карточки SCA Secrets SAST" --type text --top 1

# готовый набор "вопрос -> запрос -> координаты" по теме ASOC-UI:
# см. QUERIES-ASOC.md

# масштаб: все сессии (осторожно, история большая) или первые N
./bin/bleve-spike index --all --limit 10 --index data/all.bleve
```

Переменные окружения:

- `MEMORY_SQLITE` - путь к `opencode.db` (default `~/.local/share/opencode/opencode.db`)
- `MEMORY_INDEX` - каталог индекса (default `./data/index.bleve`)

## Как устроен индекс

Модель документа: **один документ на одну часть сессии** (тот же уровень
гранулярности, что у чанков в opencode-rag). Doc ID = `part.id`, поэтому
повторная индексация той же части перезаписывает документ, а не плодит дубли.

В индексе лежит только то, что нужно для поиска и для возврата координат.
Полные оригиналы не дублируются: их отдаёт `read` из SQLite по
`(session_id, position)`, ровно как `memory_read`.

Маппинг (`index.go`, `buildMapping`):

| Поле            | Тип      | Анализатор | Indexed | Stored | Зачем                                  |
|-----------------|----------|------------|---------|--------|----------------------------------------|
| `content`       | text     | `ru`       | да      | да*    | поиск: морфология, стоп-слова           |
| `content_exact` | text     | `simple`   | да      | нет    | точные слова без стемминга (`tree.sql`)  |
| `content_ident` | text     | `ident`    | да      | нет    | идентификаторы целиком (`index.html`)    |
| `session_id`    | keyword  | keyword    | да      | да     | координата + фильтр                     |
| `position`      | numeric  | -          | да      | да     | координата                              |
| `project_path`  | keyword  | keyword    | да      | да     | фильтр по проекту                       |
| `part_type`     | keyword  | keyword    | да      | да     | фильтр `text/tool/reasoning/patch`      |
| `role`          | keyword  | keyword    | да      | да     | роль сообщения                          |
| `tool`          | keyword  | keyword    | да      | да     | имя инструмента                         |
| `command`       | text     | `simple`   | да      | да     | команда для tool-частей                 |
| `snippet`       | text     | -          | нет     | да     | короткая выдача, если content не хранится |
| `time_created`  | numeric  | -          | да      | да     | фильтр "не старше N" и сортировка       |

\* `content.Store` управляется флагом `--store-content` (default true). При
`false` фрагменты подсветки недоступны, в выдаче остаётся `snippet`.

Почему два текстовых поля: `ru`-анализатор приводит слова к основе
(`матчинга` -> `матчинг`), что даёт морфологию, но может портить латинские
идентификаторы. `content_exact` с `simple` (только lowercase) ловит точные
имена. Поиск по умолчанию - дизъюнкция обоих полей.

Координаты хранятся как **stored** поля, поэтому bleve возвращает их прямо в
хите (`hit.Fields["session_id"]`, `hit.Fields["position"]`), без второго
запроса. Именно это и требовалось: локальный индекс умеет возвращать
`session_id` + `position`, как PG-индекс.

## Модель поиска

Фильтры (`session_id`, `project_path`, `part_type`, `time_created >= X`)
добавляются к любому режиму через `ConjunctionQuery`. Подсветка - высоклайтер
`ansi` (нужен blank-import пакета, иначе `no highlighter with name or type
'ansi' registered`). Сортировка - по скору (default) или `--sort time`.

Режимы (`--mode`):

| mode     | Запрос bleve                        | Поле            | Когда нужен                         |
|----------|-------------------------------------|-----------------|-------------------------------------|
| `match`  | OR(content, content_exact)          | content + exact | дефолт: морфология + точные слова (ident не в дефолте) |
| `phrase` | MatchPhraseQuery                    | content (ru)    | точная фраза с порядком слов        |
| `ident`  | MatchQuery                          | content_ident   | идентификатор целиком               |
| `term`   | TermQuery (без анализа)             | content_ident   | один точный токен (`tool_name`)     |
| `prefix` | PrefixQuery                         | content_ident   | `asocvuln` -> `asocvulnerability`   |
| `fuzzy`  | FuzzyQuery (edit distance 1)        | content_ident   | опечатки (`swager` -> `swagger`)    |
| `regexp` | RegexpQuery (якорная, от начала)    | content_ident   | `.*\.yaml`, `cve-20[0-9]{2}-[0-9]+` |
| `wildcard` | WildcardQuery (`*`, `?`)          | content_ident   | `*vulnerability`, `*api*key*`       |
| `qs`     | QueryStringQuery                    | любой           | `content:foo AND tool:bash, +фраза` |

Поле `content_ident` (анализатор `ident`): regexp-токенайзер с шаблоном
`[A-Za-z0-9_./:@-]+` + `to_lower`. Держит неделимыми `index.html`,
`tree.sql`, `tool_name`, `docs/openapi/swagger.yaml`, `CVE-2023-1234`, поэтому
`term`/`prefix`/`fuzzy`/`regexp`/`wildcard` работают по целым идентификаторам.

Важно: `camelCase`-фильтр в этот анализатор НЕ добавлен - он дополнительно
режет токен по не-алфанумерике (`tool_name` -> `tool`, `_`, `name`) и ломает
точный `term`.

Проверено на сессии `ses_f7588cd6dffeW430jRLkmAHt6Y`:

```bash
search "tool_name"    --mode term     # pos 1076, 1072, 1082 (целый токен)
search "index.html"   --mode term     # pos 342
search "matched_cod"  --mode fuzzy    # pos 598, 602, 610 (опечатка)
search "asocvuln"     --mode prefix   # pos 117, 561, 514
search "cve-20[0-9]{2}-[0-9]+" --mode regexp   # pos 326, 313, 318
search "*vulnerability" --mode wildcard        # pos 494, 349, 6
```

## Результаты замеров (сессия ses_f7588cd6dffeW430jRLkmAHt6Y)

| Метрика                          | Значение            |
|----------------------------------|---------------------|
| частей в сессии                  | 1149                |
| документов в индексе             | 620 (остальное - step-start/finish/compaction) |
| время сборки индекса             | ~280-360 мс         |
| размер с `--store-content=true`  | 4.6 MiB             |
| размер с `--store-content=false` | 4.4 MiB             |
| латентность поиска (процесс целиком) | ~10 мс          |

Репозиторий целиком (для оценки масштаба): 1207 сессий, 24 проекта,
308 850 частей, 74 432 сообщения. При индексации ~55% частей (по типам
целевой сессии) это порядка 170 тыс. документов.

## Выводы

1. bleve подходит как локальный полнотекстовый индекс: координаты
   `(session_id, position)` возвращаются напрямую, `read` по ним работает.
2. Хранение полного `content` почти не влияет на размер (+4%): в индексе
   доминируют posting-списки и структуры сегментов, а не stored-поля. Значит
   хранить текст можно, не боясь раздувания.
3. Полная история - это единицы гигабайт (плотность сильно зависит от сессии:
   7.6 KiB/док на целевой против 28 KiB/док на 10 свежих сессиях с большими
   выводами команд). Для локального диска приемлемо, но это не "маленький
   индекс".
4. bleve чисто лексический: он не заменяет pgvector-семантику. Разумные
   варианты - (а) добавить bleve как быстрый локальный lexical-слой рядом с
   PG, (б) сделать полностью локальный режим без Postgres и Ollama, где bleve
   единственный источник поиска.

## Что учесть при переносе в opencode-rag

- **Инкрементальность.** Doc ID = part id, повторный `Index` перезапишет
  документ, но удалять устаревшие части (сообщения, которые правились) bleve
  сам не станет. Нужен аналог `sync_state`: по `session.time_updated` решать,
  какие сессии переиндексировать, и перед этим удалять документы сессии
  (TermQuery по `session_id` + DeleteByQuery, или проекция `position`).
- **Блокировка каталога.** Один каталог индекса bleve держит эксклюзивно:
  индексатор и MCP-сервер не откроют его одновременно из разных процессов.
  Либо всё в одном процессе, либо один пишущий + читающие через snapshot.
- **Российский анализатор и латиница.** Стеммер `ru` может искажать
  идентификаторы, поэтому второе поле `content_exact` обязательно.
- **Позиции.** Координата `position` должна считаться так же, как в
  opencode-rag (`message.time_created`, затем `part.rowid`), иначе search и
  read разъедутся.
- **Batch.** Индексировать через `idx.NewBatch()` пачками, иначе на полной
  истории будет очень медленно.
