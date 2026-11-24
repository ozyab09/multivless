# Multi-Exit VLESS Gateway

Прокси-шлюз: держит все VLESS-серверы активными одновременно и выбирает
исходящий сервер **независимо для каждого нового TCP-соединения**. Клиенты
подключаются к одному SOCKS5-порту; прямого выхода в интернет нет — если
живых серверов не осталось, новые соединения отклоняются (fail-closed).

Реализация следует `TECHNICAL_SPECIFICATION.md`.

**Статус: этапы 1–5 завершены в Docker-варианте; DEB/systemd и CLI-управление
пока не входят в поставку.**

---

## Что уже работает

- Импорт VLESS URI: TCP/TLS/REALITY, WS, gRPC, HTTPUpgrade; параметры
  `sni`, `fp`, `pbk`, `sid`, `flow`, `host`, `path`, `serviceName`, `alpn`.
- Генерация конфигурации sing-box, `sing-box check` перед запуском,
  перезапуск процесса после падения. Узлы с транспортом, который не умеет
  запущенная сборка, получают статус `ERROR` с причиной и не участвуют в
  проверках и балансировке.
- Транспорт XHTTP: образ использует форк [sing-box-lx](https://github.com/Leadaxe/sing-box-lx)
  (сборка sing-box 1.14.4 с `with_xhttp`), потому что upstream sing-box
  XHTTP не поддерживает. Ссылки `type=xhttp` импортируются как обычно;
  режим `auto` приводится к `stream-one`, который Xray-серверы принимают.
- SOCKS5 TCP CONNECT: IPv4, IPv6, домены, RFC 1929, лимиты и таймауты,
  лимит соединений на пользователя.
- Балансировка каждого соединения: `adaptive`, `round-robin`, `random`,
  `least-latency`, `least-connections`, `weighted`.
- Health check **через туннель** (реальный HTTP-запрос через VLESS-outbound),
  пороги online/offline, RTT как EMA.
- Fail-closed: пустой пул → SOCKS5 отвечает ошибкой; `direct` не генерируется.
- SQLite (`modernc.org/sqlite`, без CGO): серверы, подписки, пользователи,
  настройки.
- Подписки: периодическое обновление, ETag/304, лимит размера, защита от
  SSRF, атомарная замена набора серверов, сохранение последней валидной версии.
- Поддержка панелей с привязкой к устройству: при загрузке отправляется
  стабильный `X-HWID` (генерируется один раз и хранится в SQLite). Без него
  такие панели отдают заглушку `vless://…@0.0.0.0:1`; такие ответы теперь
  отклоняются с понятной ошибкой и не попадают в пул.
- Пользователи SOCKS5 в базе, пароли — Argon2id PHC; сид из YAML при пустой
  таблице.
- REST API + встроенный React Web UI (один бинарник, SPA fallback).
- Учёт соединений и байт по пользователю/серверу, Prometheus-метрики.
- Web UI: график входящего/исходящего трафика (обновление раз в 3 секунды,
  только при видимой вкладке), история закрытых соединений за 60 секунд,
  отдельная вкладка логов соединений и DNS-запросов с флагом страны.
- Подписки редактируются: имя, URL, интервал обновления и частота
  health-check серверов (по умолчанию 30 секунд) — индивидуально для каждой
  подписки.
- DNS-атрибуция: домены целей резолвятся локально (только для логов, маршрут
  по-прежнему резолвит exit-сервер) и раскрашиваются по `geoip.dat`.
- Torrent-bypass на уровне пользователя: BitTorrent-потоки такого
  пользователя уходят напрямую, остальной трафик — через VLESS.
- Структурированные JSON-логи; секреты не попадают в API и логи.

## Чего пока нет

- CLI-управление серверами/подписками (только API/UI и YAML).
- DEB-пакет, `install.sh`, systemd-юнит.
- WebSocket-обновлений в UI (сейчас polling; график — раз в 3 с, статусы —
  раз в 30 с).
- Истории метрик и агрегации в SQLite (логи и история соединений — в памяти).
- Шифрования секретов внутри SQLite (файл создаётся с правами `0600`).
- Реального измерения Throughput (score фиксирован на 50).

---

## Быстрый старт в Docker

```bash
# Страновые флаги в логах (файл 23 МБ, в git не попадает).
./scripts/fetch-geoip.sh

# Токен администратора; без него используется local-dev-token из compose.
export MULTIVLESS_API_TOKEN="$(openssl rand -hex 16)"
docker compose up --build -d
```

- Web UI: <http://localhost:8080> — вставить токен, вкладка «Подписки» →
  добавить URL подписки (base64 или список `vless://`). Ссылку подписки нужно
  вставлять именно туда: во вкладке «Серверы» ожидается одиночный `vless://`
  URI, и на `http(s)://` API ответит подсказкой.
- SOCKS5: `127.0.0.1:1080`, пользователь `demo` / `changeme` из
  `deploy/docker/config.yaml` (смените пароль на вкладке «Пользователи»).
- Логи: `docker compose logs -f`; состояние в томе `multivless-data`.
- Порты опубликованы только на localhost. Образ собирается для архитектуры
  из `DOCKER_DEFAULT_PLATFORM` (по умолчанию — архитектура Docker-хоста);
  runtime-база Debian, потому что бинарник sing-box собран под glibc.
  Движок — `sing-box-lx` (`SING_BOX_REPO`/`SING_BOX_VERSION` в Dockerfile
  можно переопределить); контрольная сумма сверяется с `SHA256SUMS` релиза.

Проверка через SOCKS5:

```bash
curl --socks5-hostname 127.0.0.1:1080 -U demo:changeme https://ifconfig.me
```

`geoip.dat` монтируется в контейнер как `/etc/multivless/geoip.dat`. Без файла
логи работают, но вместо флага будет `—`. Обновить базу: повторить
`./scripts/fetch-geoip.sh` и перезапустить контейнер.

### Память на VPS

Шлюз — один Go-процесс и один `sing-box`; основной вклад даёт Go heap, число
соединений и загруженный `geoip.dat` (~10–20 МБ RSS). В compose уже заданы
`GOMEMLIMIT=128MiB` и `GOGC=50` — мягкий лимит heap и более частый GC. Для
машины на 512 МБ этого достаточно; на 256 МБ поставьте
`GOMEMLIMIT=80MiB`, при сотнях одновременных потоков — увеличьте до 256 МБ и
уменьшите `engine.lanes` (каждый lane — отдельный loopback-inbound).

## Локальный запуск без Docker

```bash
make web          # собрать UI в internal/web/dist (нужно один раз)
make build
cp configs/config.example.yaml /tmp/multivless.yaml
# отредактировать servers/пути, при необходимости api.token
bin/multivless check-config --config /tmp/multivless.yaml
bin/multivless run --config /tmp/multivless.yaml
```

Если `api.token` пуст, при старте генерируется случайный токен и пишется в лог
один раз (`MULTIVLESS_API_TOKEN` в окружении его переопределяет).

### E2E и smoke-тесты без внешних серверов

```bash
SING_BOX=/path/to/sing-box ./scripts/e2e.sh   # транспорт, балансировка, failover
./scripts/smoke-api.sh                        # SQLite, подписка, API, UI, пользователи
```

`smoke-api.sh` поднимает два локальных VLESS-exit, HTTP-origin, сервер
подписки и шлюз, затем проверяет: 401 без токена, отдачу UI, импорт подписки
(2 сервера), health-check через туннель, ONLINE-статусы, Prometheus, создание
пользователя и вход по Argon2id, failover после убийства exit, счётчики
соединений.

---

## Web UI и REST API

Вкладки:

- **Обзор** — карточки статуса, график скорости ↓/↑ (3 с, только при открытой
  вкладке) и последние соединения.
- **Серверы** — статус, RTT, включение/вес/удаление, ручное добавление URI;
  список обновляется каждые 30 секунд.
- **Подписки** — добавление, редактирование (имя, URL, интервал обновления,
  частота проверки серверов), обновление, удаление.
- **Пользователи** — SOCKS5-аккаунты, лимит соединений и переключатель
  «torrent напрямую».
- **Соединения** — активные и закрытые за последние 60 секунд.
- **Логи** — старт/конец соединений и DNS-запросы с IP и флагом страны
  (обновление раз в 3 секунды).
- **Настройки** — стратегия балансировщика.

Авторизация — `Authorization: Bearer <token>` для всех `/api/v1/*`, кроме
`/api/v1/health`.

| Метод и путь | Назначение |
| --- | --- |
| `GET /api/v1/health` | liveness, версия, запущен ли sing-box |
| `GET /api/v1/status`, `/stats` | состояние пула, стратегия, счётчики |
| `GET/POST /api/v1/servers` | список / добавить VLESS URI |
| `PATCH/DELETE /api/v1/servers/{id}` | включить, вес, удалить |
| `GET/POST /api/v1/subscriptions` | список / добавить подписку |
| `PATCH/DELETE /api/v1/subscriptions/{id}` | изменить / удалить подписку |
| `POST /api/v1/subscriptions/{id}/update` | обновить сейчас |
| `GET/POST /api/v1/users` | список / создать пользователя SOCKS5 |
| `PATCH/DELETE /api/v1/users/{id}` | пароль, лимит, torrent-bypass, вкл/выкл / удалить |
| `GET /api/v1/connections` | активные + закрытые за 60 секунд |
| `GET /api/v1/logs` | события соединений и DNS |
| `GET/PATCH /api/v1/settings` | стратегия и параметры балансировщика |
| `GET /api/v1/metrics` | Prometheus-метрики |

**Важно:** любое изменение набора серверов (подписка, добавление, удаление)
перезапускает sing-box с теми же lane-портами. Активные соединения при этом
рвутся; новые сразу идут через обновлённый пул. Это осознанный компромисс —
sing-box не умеет добавлять outbound на лету.

---

## Как устроен выбор outbound (ключевое решение)

Спецификация требует собственный SOCKS5-слой, но у sing-box нет штатного
«дай это соединение в outbound X». Реализовано и проверено E2E:

```
клиент ──SOCKS5──► gateway (Go)
                     │ 1. выбрать сервер (балансировщик)
                     │ 2. взять свободную lane, залочить её
                     │ 3. PUT /proxies/{group} → выбрать outbound
                     │ 4. SOCKS5-handshake в lane-inbound sing-box
                     ▼
              sing-box: lane-inbound ──route(inbound)──► selector-группа ──► VLESS outbound
```

- В конфиге sing-box N lane-inbound'ов (loopback-порты) и N selector-групп,
  каждая со всеми VLESS-outbound'ами.
- Переключение — через Clash API (`experimental.clash_api`), единственный
  поддерживаемый способ выбрать outbound без вмешательства во внутренности.
- Lane занят одним соединением, поэтому пара «select + dial» без гонки; после
  установления соединения outbound закреплён за ним до закрытия.
- Health check использует отдельные lane'ы, не конкурируя с трафиком.

Компромисс: Clash API экспериментальный, но публичный. Это осознанное
отступление от «только документированные интерфейсы», зафиксированное как
архитектурное решение; альтернативы (inbound на сервер, процесс на сервер) не
масштабируются на 1000 узлов.

### Torrent-bypass

Пользователю можно включить «torrent напрямую». Для таких соединений
выделены отдельные lane-группы: в них sing-box сначала выполняет `sniff`, а
затем правило `protocol: bittorrent → outbound: direct`. Остальные протоколы
из этих же lane идут в обычный VLESS-selector. Прямой outbound создаётся
**только** при наличии torrent-lane и недоступен остальному трафику —
fail-closed для не-torrent соединений сохраняется.

Ограничение: SOCKS5-шлюз поддерживает только TCP CONNECT, поэтому напрямую
уходит TCP-BitTorrent; UDP/DHT через этот шлюз не проксируется в принципе.

---

## Конфигурация

Полный пример — `configs/config.example.yaml`, Docker-вариант —
`deploy/docker/config.yaml`.

| Секция | Назначение |
| --- | --- |
| `engine` | путь к `sing-box`, путь к генерируемому конфигу, Clash API, число lane'ов |
| `storage` | путь к SQLite |
| `api` | вкл/выкл, адрес, порт, admin-токен |
| `subscriptions` | интервал, таймаут, максимальный размер, `allow_private_hosts` |
| `socks5` | адрес, порт, авторизация, лимиты, начальные пользователи |
| `balancer` | стратегия, веса adaptive, `max_attempts` |
| `health_check` | интервал, таймаут, пороги, конкурентность, URL |
| `geoip` | путь к `geoip.dat` для флагов в логах (необязательно) |
| `servers` / `sources` | VLESS URI вручную / файлы с URI или base64 |

`api.listen` вне loopback разрешён только при включённой авторизации SOCKS5.
`allow_private_hosts: true` отключает защиту от SSRF — только для локальных
тестов.

## Структура

```
cmd/multivless/           run, check-config, healthcheck, version
internal/config/          загрузка и валидация YAML
internal/subscriptions/   менеджер подписок + parser/ VLESS URI
internal/storage/         SQLite (schema.sql), CRUD
internal/security/        Argon2id
internal/users/           аутентификация, сид из конфига
internal/api/             REST, Prometheus, auth
internal/web/             embed UI + dist/
internal/servers/         реестр, статусы, метрики
internal/balancer/        стратегии
internal/engine/singbox/  конфиг, процесс, Clash API, lane-dial
internal/health/          проверки через туннель
internal/router/          выбор сервера, учёт соединений и байт
internal/socks5/          SOCKS5-шлюз
internal/connections/     активные соединения + история 60 с
internal/eventlog/        кольцевой буфер событий для вкладки «Логи»
internal/geoip/           парсер geoip.dat (страновые флаги)
internal/app/             сборка компонентов
web/                      React + TypeScript + Vite (исходники UI)
scripts/                  e2e.sh, smoke-api.sh, fetch-geoip.sh
deploy/docker/            конфиг для контейнера
docs/                     GitHub Pages (лендинг)
.github/workflows/        CI и деплой Pages
Dockerfile, docker-compose.yml
```

UI написан на обычном CSS, без Tailwind (упрощение относительно ТЗ).

## Проверено и не проверено

Проверено: `go vet`, `go test ./...` (включая geoip.dat, историю соединений,
XHTTP- и torrent-lane в конфиге), `check-config`, `sing-box check`
1.14.4-lx.36 (включая XHTTP-конфиг, живое соединение через XHTTP-узел и
правила `sniff` → `protocol: bittorrent` → `direct`),
`scripts/e2e.sh` (балансировка, failover, fail-closed, recovery),
`scripts/smoke-api.sh` (API, UI, подписка, SQLite, пользователи, метрики),
`docker compose build`/`docker run` с non-root и `read_only`: UI, API, импорт
сервера через API, запуск sing-box в контейнере и проксирование через SOCKS5.

Не проверено: VLESS/REALITY/WS/gRPC против реальных панелей и внешних
подписок, нагрузка 1000 серверов / 10 000 соединений, Ubuntu 22.04/24.04,
DEB/systemd, поведение при обрыве сети.

## Публикация на GitHub

В репозитории уже есть:

- `.github/workflows/ci.yml` — `go vet`, `go test`, сборка Go, `npm ci && npm run
  build` для UI на каждый push/PR.
- `.github/workflows/pages.yml` — публикует `docs/` на GitHub Pages (в
  настройках репозитория выберите **Settings → Pages → Source: GitHub
  Actions**).
- `Dockerfile` + `docker-compose.yml` — готовый образ; при публикации можно
  добавить job для `ghcr.io` (не включён, чтобы не требовать прав на пакеты).

Порядок первой публикации:

```bash
git init
git add . && git commit -m "Initial commit"
# создайте пустой репозиторий на GitHub и выполните:
git remote add origin git@github.com:<owner>/<repo>.git
git branch -M main
git push -u origin main
```

`geoip.dat`, `.tools/`, `run/`, `bin/` и `*.log` исключены через `.gitignore`.
Не коммитьте `deploy/docker/config.yaml` с реальными токенами — используйте
`MULTIVLESS_API_TOKEN` в окружении. Перед публикацией выберите лицензию
(например, MIT для собственного кода; бинарник sing-box-lx остаётся GPLv3 и
скачивается при сборке).

## Безопасность

- SOCKS5 и API по умолчанию слушают loopback; внешний адрес API требует
  авторизации SOCKS5, в Docker порты опубликованы только на `127.0.0.1`.
- Пароли — Argon2id; в API и логи попадают только имена пользователей.
- Токен сравнивается за постоянное время; без токена API отвечает 401.
- Подписки: лимит размера, таймаут, запрет приватных адресов и редиректов на
  них, последняя валидная версия сохраняется при ошибке загрузки.
- VLESS-секреты не попадают в ID сервера, логи и ошибки; сгенерированный
  конфиг sing-box и файл базы создаются с правами `0600`.
- Прямой fallback отсутствует для обычного трафика: outbound `direct`
  создаётся только если включён torrent-bypass, и только правило
  `protocol: bittorrent` может направить в него соединение.
- Контейнер работает от uid 10001, `read_only`, `cap_drop: ALL`,
  `no-new-privileges`.
- sing-box-lx (форк sing-box) распространяется под GPLv3: публикуя образ с
  ним, учитывайте
  обязательства этой лицензии.
