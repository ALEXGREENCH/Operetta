# Документация по серверу Operetta

## Обзор
Operetta — это переосмысленная на Go реализация старого прокси Opera Mini 1.x/2.x/3.x. Она принимает null-разделённые POST-запросы клиентов Opera Mini 2.0 (включая модификацию 2.06 DG-SC), загружает исходный HTML, преобразует его в поток OBML/OMS v2 и возвращает аппаратному клиенту.

## Структура репозитория
- `cmd/operetta/` — CLI-входная точка: читает флаги/переменные окружения, создаёт `proxy.Config`, запускает `proxy.New(cfg)` внутри `net/http.Server`.
- `internal/proxy/` — HTTP-слой: конфигурация (`config.go`), обработчики (`handlers.go`), журналирование, хранилище предпочтений рендера, per-client cookie jar, кэш пагинации, загрузчик JSON-override’ов по хосту.
- `internal/proxy/url.go` — вспомогательные функции для нормализации Opera-style URL и построения action/get.
- `oms/` — движок рендеринга, разбитый на модули (`page.go`, `normalize.go`, `cache_disk.go`, ...): загрузка HTML, CSS-эвристики, обход DOM, генерация OBML, конвейер изображений и финализация OMS.
- `config/sites/` — примеры JSON-override’ов (`mode`, `headers`), директория переопределяется `OMS_SITES_DIR`.
- `docs/` — справочные материалы (OBML, протокол, это руководство).
- `dist/`, `build.ps1`, `build.sh`, `Makefile` — скрипты сборки и упаковки.

## Архитектура выполнения
1. `cmd/operetta/main.go` читает `-addr` или `PORT`, строит `proxy.Config` (`proxy.DefaultConfig()` + ваши правки) и создаёт `*proxy.Server` через `proxy.New(cfg)`.
2. `Server` регистрирует маршруты: `/` (Opera Mini POST), `/fetch` (ручной запрос), `/validate` (диагностика), `/ping` (health-check). Поверх ставится логгер `withLogging`.
3. `handleRoot` парсит null-разделённые пары (`parseNullKV`), нормализует URL (`normalizeObmlURL`), собирает `oms.RenderOptions` из клиентских подсказок (`k`, `d`, `j`, auth-токены) и вызывает `loadPage`.
4. `loadPage` подмешивает per-site настройки (`site_config.go`), выбирает `oms.LoadPageWithHeadersAndOptions` или `oms.LoadCompactPageWithHeaders`, использует per-client cookie jar и кэш пагинации, после чего возвращает `*oms.Page`.
5. Ответ: `Content-Type: application/octet-stream`, точный `Content-Length`, `Connection: close`. Через `dumpOMS` логируются первые байты, `Set-Cookie` проксируются, а packed OMS отдаётся клиенту.

## Процесс Opera Mini
Запрос клиента — POST с `Content-Type: application/xml`, тело — null-разделённые `key=value`. Основные поля:

| Ключ | Назначение |
|------|------------|
| `k` | Предпочтительный MIME изображений (`image/jpeg` и т.п.). |
| `o` | Версия шлюза (OM 2.x → 280, OM 3.x → 285). |
| `u` | URL/путь (`/obml/<scheme>/<url>`); преобразуется в абсолютный URL. |
| `q`, `y`, `D` | Языковые настройки. |
| `v`, `i` | User-Agent клиента/«десктопный» UA. |
| `d` | Capability-блок: ширина, высота, количество цветов, heap, флаги качеcтва изображений. |
| `c`, `h` | Auth-коды, которые нужно отразить в ответе. |
| `j` | URL-кодированное тело формы (при сабмите). |
| `w` | Индикатор пагинации (`page;isLast`). |

Ответ — `HTTP/1.1 200 OK` с `application/octet-stream`, однозначным `Content-Length` и закрытием соединения. Тело — packed OMS/OBML v2.

## Конфигурация
Часть настроек можно задавать через окружение:

| Переменная | Описание |
|------------|----------|
| `PORT` | Порт для `cmd/operetta` (по умолчанию `:8081`, можно переопределить флагом `-addr`). |
| `OMS_BOOKMARKS_MODE` | `remote/pass/passthrough` — оставить портал opera-mini.ru, любое другое значение — отдать локальный список закладок. |
| `OMS_BOOKMARKS` | Список `Название|URL` через запятую для локальной страницы закладок. |
| `OMS_SITES_DIR` | Папка с JSON-override’ами (по умолчанию `config/sites`). |
| `OMS_IMG_CACHE_DIR` / `OMS_IMG_CACHE_MB` | Директория и размер дискового кэша изображений (мегабайты, по умолчанию 100). |
| `OMS_IMG_DEBUG` | `1` — подробные логи загрузки/конверсии изображений. |
| `OMS_TAGCOUNT_MODE` / `OMS_TAGCOUNT_DELTA` | Тонкая настройка подсчёта тегов для совместимости со старыми клиентами. |

Программный способ:
```go
cfg := proxy.DefaultConfig()
cfg.Bookmarks = []proxy.Bookmark{
    {Title: "Яндекс", URL: "https://yandex.ru"},
    {Title: "Википедия", URL: "https://ru.wikipedia.org"},
}
srv := proxy.New(cfg)
http.ListenAndServe(":8081", srv)
```

## Рендеринг OMS
Модуль `oms` отвечает за:
- Загрузку HTML (`LoadPage*`), декодирование и нормализацию кодировок.
- CSS-эвристики: вычисление цвета текста/фона, упрощение стилей.
- Обход DOM (рекурсивный `walkRich`) и генерацию OBML-тегов (`T`, `L`, `S`, `B`, `+`, формы и т.д.).
- Конвейер изображений: встраивание (`I`), плейсхолдеры (`J`), кэш на диске и в памяти.
- Пагинацию: `splitByTags`, генерацию навигационных ссылок, хранение packed снапшотов для повторного использования.
- Финализацию/нормализацию: подсчёт тегов/строк, запись заголовка OMS v2, дефлейт, опциональную нормализацию `NormalizeOMS`/`NormalizeOMSWithStag`.

## Диагностика
- `dumpOMS` логирует магию OMS, размер и первые байты.
- `/validate?url=...` генерирует полный и «compact» варианты, прогоняет `analyzeOMS` и отдаёт JSON со статистикой.
- С HTML-формы на `/` удобно тестировать прокси без настоящего клиента.
- Установите `OMS_IMG_DEBUG=1`, чтобы видеть подробности загрузки/конвертации изображений.

## Ограничения
- CSS ограничен базовыми стилями; сложные макеты, float’ы, media queries игнорируются.
- Формы: полностью поддерживается GET, POST проксируется в пределах `RenderOptions.FormBody`; multipart и файлы отсутствуют.
- Изображения могут быть понижены в качестве или заменены плейсхолдером в зависимости от `MaxInlineKB` и предпочтений клиента.
- Транспорт — всегда HTTP/1.1 с закрытием соединения; TLS требует внешнего терминатора (reverse proxy/stunnel).

## Дополнительные материалы
- `docs/OBML.md` — подробности формата OBML и пагинации.
- `docs/oms_protocol.md` — описание оригинального протокола Opera Mini → proxy.
- [grawity/obml-parser](https://github.com/grawity/obml-parser/blob/master/obml-format.md) — неофициальная спецификация поздних версий OBML.
