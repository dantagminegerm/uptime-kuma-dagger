# Uptime Kuma — локальный контур мониторинга под управлением Dagger

Стенд разворачивает Uptime Kuma с MariaDB и полноценный контур наблюдаемости
(Prometheus + Grafana + экспортёры). Весь жизненный цикл — деплой, настройка
Grafana через API, проверка здоровья и удаление — выполняется функциями
Dagger-модуля на Go, который управляет хостовым Docker через проброшенный
`/var/run/docker.sock`.

```bash
dagger call deploy-all
```

## Архитектура

```
                               host (127.0.0.1 only)
                 :3001                 :3000              :9090
                   │                     │                  │
┌─ Слой 1: uptime-app ────────┐  ┌─ Слой 2: uptime-monitoring ─────────────────┐
│                             │  │                                              │
│  uptime-kuma ──► mariadb    │  │  grafana ──► prometheus ──► node-exporter    │
│   (2.5.6)    backend  (11.8)│  │                    │   ├──► cadvisor         │
│      │      (internal)  │   │  │                    │   ├──► mysqld-exporter ─┼─┐
│      │                  │   │  │                    │   └──► blackbox-exporter┼─┤
└──────┼──────────────────┼───┘  └────────────────────┼──────────────────────────┘ │
       │                  │                           │                            │
═══════╧══════════════════╧═══ platform-observability (internal) ═══════════════════╧═══
                    HTTP/TCP-пробы и сбор метрик идут через эту сеть
```

| Слой | Compose-проект | Сервисы |
|---|---|---|
| 1 — приложение и БД | `app/docker-compose.yml` (`uptime-app`) | `uptime-kuma`, `mariadb` (volume `mariadb-data`) |
| 2 — наблюдаемость | `monitoring/docker-compose.yml` (`uptime-monitoring`) | `prometheus`, `grafana`, `node-exporter`, `cadvisor`, `mysqld-exporter`, `blackbox-exporter` |

Слои независимы: у каждого свой compose-проект, свои volume и сети. Общая у
них только внешняя сеть `platform-observability`, которую создаёт Dagger. Её
можно поднимать и удалять в любом порядке: если приложения нет, мониторинг
просто покажет его как DOWN.

### Что собирается и чем

| Требование | Источник метрик |
|---|---|
| Состояние хоста (CPU, RAM, диск, load) | `node-exporter` |
| Ресурсы контейнеров (CPU, RAM, сеть, лимиты) | `cadvisor` |
| Статус MariaDB (`mysql_up`, соединения, QPS, InnoDB) | `mysqld-exporter` под отдельной учёткой с минимальными правами |
| Доступность и latency веб-сервисов | `blackbox-exporter`: HTTP-пробы Uptime Kuma, Grafana и Prometheus, TCP-проба MariaDB:3306 |
| Статус Uptime Kuma | HTTP-проба, healthcheck контейнера, метрики cAdvisor |

Prometheus также загружает правила алертов (`monitoring/prometheus/rules/platform.yml`):
сервис недоступен, медленный ответ, MariaDB недоступна, нехватка соединений,
контейнер упирается в лимит памяти, высокая загрузка CPU хоста, мало места на диске.

Дашборд Grafana **Platform / Platform Health** (`grafana/dashboards/platform-health.json`):

* **Overview** — UP/DOWN Uptime Kuma и MariaDB, latency Kuma, uptime БД, доля живых таргетов, число контейнеров;
* **Service availability & latency** — таймлайн статусов проб и время ответа;
* **Containers** — CPU, память, сеть по контейнерам, память относительно лимита (с фильтром `$container`);
* **MariaDB** — соединения и `max_connections`, QPS и медленные запросы, InnoDB buffer pool;
* **Docker host** — CPU, RAM, корневая ФС, load average.

## Dagger-модуль

Модуль лежит в корне репозитория (`dagger.json`, исходники — в `dagger/`, Go SDK).

| Команда | Что делает |
|---|---|
| `dagger call deploy-app` | Создаёт сеть, поднимает MariaDB и Uptime Kuma (`up --wait` ждёт healthcheck), затем идемпотентно создаёт/обновляет учётку `exporter` в MariaDB |
| `dagger call deploy-monitoring` | Собирает образ Prometheus с конфигом (во время сборки конфиг проверяется `promtool`) и поднимает Prometheus, Grafana и экспортёры |
| `dagger call configure-grafana` | Через Grafana HTTP API создаёт или обновляет data source (и проверяет его через `/health`), папку и дашборды из `grafana/` |
| `dagger call deploy-all` | Всё перечисленное выше по порядку, затем `validate` |
| `dagger call validate` | Проверяет здоровье контейнеров, что все таргеты Prometheus в состоянии `up`, `probe_success` и `mysql_up`, HTTP Uptime Kuma, data source и дашборд в Grafana. При любой ошибке выходит с ненулевым кодом |
| `dagger call destroy [--volumes]` | Удаляет оба слоя; с `--volumes` удаляет и данные |

### Как Dagger управляет Docker

Каждая функция запускает контейнер `docker:29.9-cli` с хостовым сокетом,
подключённым через `WithUnixSocket` (аргумент модуля `--sock`), и выполняет
`docker compose` для нужного слоя. В контейнер монтируются только `app/`,
`monitoring/` и `grafana/`: фильтр `+ignore` не отправляет в движок Dagger ни
`.secrets/`, ни `.git`.

Compose работает внутри контейнера, а демон — на хосте, поэтому bind-mount
файлов репозитория не работал бы: пути разрешались бы на хосте. Поэтому
конфиг Prometheus вшивается в локально собираемый образ (`build:` передаёт
контекст по сокету), секреты передаются как compose `secrets` из окружения,
а у blackbox-exporter используется встроенный конфиг (модули `http_2xx` и
`tcp_connect`).

`configure-grafana` и `validate` обращаются к Grafana, Prometheus и Uptime
Kuma на `localhost` хоста через Dagger-туннели: аргументы-сервисы
`--grafana=tcp://localhost:3000` и т. п. Поэтому порты наружу публикуются
только на `127.0.0.1`.

Все функции помечены `+cache="never"`, а exec-шаги получают cache-buster,
потому что работают с живым состоянием хоста и не должны браться из кэша Dagger.

### Аргументы и `.env`

Все аргументы модуля по умолчанию берутся из `.env` в корне
([Dagger local defaults](https://docs.dagger.io/features/local-defaults)).
Поэтому команды запускаются без флагов. В `.env` **нет значений секретов**,
только ссылки на провайдеры:

```dotenv
SOCK=/var/run/docker.sock
GRAFANA=tcp://localhost:3000
DbPassword=cmd://"sh scripts/secret.sh mariadb_password"
...
```

Ключи сопоставляются с именами аргументов без учёта регистра, но
**без подчёркиваний**: `DbPassword`, а не `DB_PASSWORD`. Подчёркивание Dagger
считает разделителем `FUNCTION_ARG`.

`scripts/secret.sh` при первом обращении генерирует случайный 32-символьный
пароль в `.secrets/<name>` (каталог в `.gitignore`, права `0600`), а дальше
возвращает его же. Любое значение можно переопределить флагом:

```bash
dagger call --grafana-password=env://MY_GRAFANA_PASSWORD deploy-monitoring
```

## Безопасность

* Секреты нигде не хранятся в репозитории и не попадают в логи Dagger: это тип `Secret`, они передаются через `WithSecretVariable`.
* MariaDB и Grafana читают пароли из файлов compose secrets (`*_FILE` / `__FILE`), а не из переменных окружения.
* У MariaDB нет опубликованных портов. Сети `backend` и `platform-observability` помечены `internal`: без выхода в интернет и без доступа с хоста. Выход наружу есть только у Uptime Kuma (ей нужно проверять внешние сайты) и у Prometheus/Grafana.
* Наружу открыты только UI-порты, и только на `127.0.0.1`.
* `mysqld-exporter` подключается под учёткой `exporter` с правами `PROCESS, REPLICATION CLIENT, SLAVE MONITOR` и `SELECT` только на `performance_schema`, с ограничением `MAX_USER_CONNECTIONS 3`. Доступа к данным Uptime Kuma у неё нет.
* В Grafana отключены регистрация, анонимный доступ, телеметрия и gravatar. Cookie выставляются с `SameSite=strict`.
* Версии всех образов зафиксированы, у контейнеров есть лимиты памяти (у приложения) и ротация логов.

## Быстрый старт

Требования: Docker Engine с Compose v2 (или Docker Desktop / WSL2) и Dagger CLI ≥ 0.21.
Ваш пользователь должен иметь доступ к `/var/run/docker.sock`
(`sudo usermod -aG docker $USER`, затем перелогиниться). Команды запускаются
из корня репозитория: `.env` ссылается на `scripts/secret.sh` по относительному пути.
Проверено на Ubuntu 24.04, Docker 29.9, Dagger 0.21.10: чистый `deploy-all`
при скачанных образах проходит примерно за 45 секунд.

```bash
dagger call deploy-all
```

После завершения:

* Uptime Kuma — <http://localhost:3001> (при первом входе создайте администратора)
* Grafana — <http://localhost:3000>, логин `admin`, пароль: `cat .secrets/grafana_admin_password`
* Prometheus — <http://localhost:9090>

Слои можно поднимать по отдельности:

```bash
dagger call deploy-app
dagger call deploy-monitoring
dagger call configure-grafana
dagger call validate
```

Данные MariaDB, Uptime Kuma, Prometheus и Grafana хранятся в именованных volume
и переживают перезапуск и повторный деплой.

```bash
dagger call destroy            # удалить контейнеры, данные оставить
dagger call destroy --volumes  # удалить всё вместе с данными
```

## Структура

```
.
├── dagger.json                 # Dagger-модуль (Go SDK, исходники в dagger/)
├── .env                        # дефолтные аргументы модуля (без секретов)
├── dagger/
│   ├── main.go                 # deploy-app, deploy-monitoring, deploy-all, destroy
│   ├── grafana.go              # configure-grafana: клиент Grafana HTTP API
│   └── validate.go             # validate: проверки контейнеров, Prometheus, Grafana
├── app/docker-compose.yml      # слой 1: Uptime Kuma + MariaDB
├── monitoring/
│   ├── docker-compose.yml      # слой 2: Prometheus, Grafana, экспортёры
│   └── prometheus/             # Dockerfile, prometheus.yml, правила алертов
├── grafana/
│   ├── datasources/            # data sources для configure-grafana
│   └── dashboards/             # дашборды для configure-grafana
└── scripts/secret.sh           # генерация и хранение локальных секретов
```

## Замечания

* Под Docker Desktop (macOS / Windows) часть метрик хоста и cAdvisor
  относится к служебной VM Docker Desktop, а не к физической машине.
* Метрики самой Uptime Kuma (`/metrics`) закрыты аутентификацией и требуют
  API-ключа, который создаётся в UI после первичной настройки. Поэтому статус
  сервиса снимается внешней HTTP-пробой. Это ближе к тому, как сервис видит
  пользователь.
