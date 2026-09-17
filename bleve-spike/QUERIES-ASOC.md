# ASOC-UI: шпаргалка "вопрос -> запрос -> координаты"

Все координаты проверены на индексе `data/index.bleve`, сессия
`ses_f7588cd6dffeW430jRLkmAHt6Y` (yellow-pages, тема ASOCVulnerability /
index.html + index.css / 3 шаблона). Формат координаты: `(session_id, position)`.

## Как спрашивать движок

Один шаг (поиск + чтение оригинала вокруг лучшего совпадения):

```bash
./bin/bleve-spike ask "ВОПРОС" --type text --top 1 --before 2 --after 6
```

Раздельно (когда нужно посмотреть несколько кандидатов):

```bash
./bin/bleve-spike search "КЛЮЧЕВЫЕ СЛОВА" --type text --limit 10
./bin/bleve-spike read   --session ses_f7588cd6dffeW430jRLkmAHt6Y --position N --before 2 --after 6
```

Правила формулировки для этого (лексического) движка:

- пиши сущности и имена полей, которые буквально есть в тексте: `index.html`,
  `index.css`, `tool_name`, `SCA`, `Secrets`, `SAST`, `matched_code`;
- морфология покрыта `ru`-анализатором (`шаблона` = `шаблон`), точные имена -
  полем `content_exact`, поэтому `index.html`/`tool_name` находятся дословно;
- отсекай шум фильтром `--type text` (мысли/ответы ассистента) либо
  `--type reasoning|tool|patch`;
- для точного синтаксиса - `--mode qs`: `content:tool_name AND content:SCA`.

## Готовый набор

| # | Вопрос | Запрос | Координаты (position) |
|---|--------|--------|------------------------|
| 1 | Где делали `index.html` + `index.css` | `ask "index.html index.css"` | 342 (запрос), 470, 475, 485, 490 |
| 2 | Где лежит HTML и как открыть | `ask "где находится index.html"` | 469, 470 |
| 3 | Какие 3 шаблона/компонента уязвимостей | `ask "3 компонента карточки SCA Secrets SAST" --type text` | 391, 495, 340 |
| 4 | По какому полю определяется шаблон | `ask "группировать по tool_name SCA Secrets SAST"` | 340 |
| 5 | ТЗ по полям для каждого компонента | `ask "Компонент 1 SCA поле в карточке откуда взято"` | 495 |
| 6 | Что в API есть, а что дотянули из дампа | `ask "Компонент 2 Secrets источник поля карточки"` | 495 |
| 7 | 3 варианта дизайна карточек | `ask "три варианта дизайна карточек компонента" --type text` | 385, 388, 404 |
| 8 | 26 вариантов уязвимостей по семействам | `ask "26 вариантов по tool_name семейства"` | 330, 294 |
| 9 | Маскировка секрета, сниппет кода | `ask "маскировать секрет matched_code"` | 340, 401 |
| 10 | recommendations (HTML-буллеты) у SAST | `ask "recommendations HTML буллеты SAST"` | 349 |
| 11 | Фильтры SCA: экосистема, inclusion_level | `ask "фильтры SCA экосистема inclusion_level"` | 340, 349, 351 |
| 12 | E2E: новые поля реально в API | `ask "matched_code message refs появились в API ответе" --type text` | 610, 1099, 556 |

Сессия для всех строк одна: `ses_f7588cd6dffeW430jRLkmAHt6Y`.

## Ответ на главный вопрос (из координат 340 и 495)

Один из 3 шаблонов определяется полем **`tool_name`** (движок - `tool_type_id`):

| Компонент | `tool_name` | Отличительные поля |
|---|---|---|
| SCA (библиотеки) | `CodeScoring SCA + Container SCA` | `library_name`+`library_version`, `cve`, `fixed_version`, `ratings`, `inclusion_level` |
| Secrets | `CodeScoring Secrets` | `source_file`+`line`+`matched_code`, `cwe=CWE-200`, severity всегда Low, тип секрета в `detailed_info` |
| SAST (код) | `Solar Sast` | `message` (заголовок), `source_file`+`line`, `matched_code`, `recommendations`, `is_master` |

Важный нюанс из `pos=495`: `matched_code`, `message`, `refs` в API-ответе
`ASOCVulnerability` отсутствовали (брались из сырого `issues.json`); позже их
добавили в схему - подтверждение в `pos=1096-1103`.
