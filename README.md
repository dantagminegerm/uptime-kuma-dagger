# Uptime Kuma — локальный контур мониторинга под управлением Dagger

Стенд разворачивает Uptime Kuma с MariaDB и полноценный контур наблюдаемости
(Prometheus + Grafana + экспортёры). Весь жизненный цикл — деплой, настройка
Grafana через API, проверка здоровья и удаление — выполняется функциями
Dagger-модуля на Go, который управляет хостовым Docker через проброшенный
`/var/run/docker.sock`.

```bash
dagger call deploy-all
```

**Содержание:**
[Подготовка окружения](#подготовка-окружения) ·
[Запуск и остановка](#запуск-и-остановка) ·
[Адреса сервисов](#адреса-сервисов) ·
[Проверка работоспособности](#проверка-работоспособности) ·
[Архитектура](#архитектура) ·
[Dagger-модуль](#dagger-модуль) ·
[Безопасность](#безопасность)

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

## Подготовка окружения

Что нужно:

| Компонент | Версия | Зачем |
|---|---|---|
| Docker Engine + Compose v2 | 24+ (проверено на 29.9) | на хосте работают оба слоя |
| Dagger CLI | 0.21.x (модуль закреплён на `v0.21.10`) | управляет жизненным циклом |
| git, curl, jq | любые | клонирование и ручные проверки |
| Go | не нужен | Go SDK собирает модуль в контейнере. Локальный Go пригодится только для правки кода в IDE |

Ресурсы: 2+ CPU, 4+ ГБ RAM и около 8 ГБ свободного диска (образы стека и движок Dagger).

### Ubuntu / Debian

1. Базовые утилиты:

   ```bash
   sudo apt-get update && sudo apt-get install -y ca-certificates curl git jq
   ```

2. Docker Engine из официального репозитория Docker
   ([документация](https://docs.docker.com/engine/install/ubuntu/)). Для Debian
   замените `ubuntu` на `debian` в обеих ссылках:

   ```bash
   sudo install -m 0755 -d /etc/apt/keyrings
   sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
   sudo chmod a+r /etc/apt/keyrings/docker.asc
   echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
   sudo apt-get update
   sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
   sudo systemctl enable --now docker
   ```

3. Доступ к Docker без `sudo`. Dagger пробрасывает в контейнеры
   `/var/run/docker.sock`, поэтому у пользователя должны быть права на сокет:

   ```bash
   sudo usermod -aG docker $USER
   newgrp docker   # или перелогиньтесь, чтобы группа применилась во всех терминалах
   ```

   Проверка (должна пройти без `sudo`):

   ```bash
   docker run --rm hello-world && docker compose version
   ```

4. Dagger CLI ([документация](https://docs.dagger.io/install)). Установка в
   `~/.local/bin`, без `sudo`:

   ```bash
   curl -fsSL https://dl.dagger.io/dagger/install.sh | DAGGER_VERSION=0.21.10 BIN_DIR=$HOME/.local/bin sh
   echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc && source ~/.bashrc
   dagger version
   ```

### macOS

```bash
brew install --cask docker   # Docker Desktop; запустите его один раз из Applications
brew install dagger/tap/dagger jq
docker compose version && dagger version
```

### Windows 10/11

1. Установите [Docker Desktop](https://docs.docker.com/desktop/setup/install/windows-install/)
   с бэкендом WSL 2 и включите интеграцию со своим дистрибутивом:
   *Settings → Resources → WSL integration*.
2. В PowerShell от администратора: `wsl --install -d Ubuntu`.
3. Дальше работайте **внутри WSL** (Ubuntu): выполните шаги 1 и 4 из раздела
   про Ubuntu. Docker Engine внутри WSL ставить не нужно, его даёт Docker Desktop.
   Репозиторий клонируйте в файловую систему WSL (`~/…`), а не в `/mnt/c/…`.

### Получение проекта

```bash
git clone https://github.com/dantagminegerm/uptime-kuma-dagger.git
cd uptime-kuma-dagger
dagger functions   # первый запуск скачает движок Dagger и соберёт модуль (1–3 мин)
```

Все команды `dagger call …` запускайте **из корня репозитория**: `.env`
ссылается на `scripts/secret.sh` по относительному пути. Пароли
сгенерируются в `.secrets/` при первом деплое.

## Запуск и остановка

```bash
dagger call deploy-all
```

Первый запуск скачивает несколько гигабайт образов, и на медленном канале это займёт время. Повторный деплой при скачанных
образах занимает около 45 секунд. Команда завершается отчётом `validate`:
`all 18 checks passed`.

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

Обычно `destroy` занимает около 10 секунд. Первый вызов после перезапуска
движка Dagger (например, после перезагрузки хоста) дольше: Dagger заново
загружает модуль. Чтобы не обращаться в Docker Hub при каждом вызове, образ
`docker:*-cli`, через который работают функции, закреплён по digest.

Контейнеры запущены с `restart: unless-stopped`, поэтому после перезагрузки
хоста платформа поднимается сама, без Dagger. Остановить её можно и без
Dagger:

```bash
docker compose -p uptime-monitoring down && docker compose -p uptime-app down
```

## Адреса сервисов

Наружу опубликованы только веб-интерфейсы, и только на `127.0.0.1`. С других
машин в сети стенд недоступен.

| Сервис | Адрес | Доступ |
|---|---|---|
| Uptime Kuma | <http://localhost:3001> | при первом входе мастер предложит создать администратора |
| Grafana | <http://localhost:3000> | логин `admin`, пароль: `cat .secrets/grafana_admin_password` |
| Grafana: дашборд | <http://localhost:3000/d/platform-health/platform-health> | папка *Platform*, дашборд *Platform Health* |
| Prometheus | <http://localhost:9090> | без авторизации |
| Prometheus: таргеты | <http://localhost:9090/targets> | состояние всех scrape-таргетов |
| Prometheus: алерты | <http://localhost:9090/alerts> | правила из `monitoring/prometheus/rules/` |

Внутренние адреса в сети `platform-observability` с хоста недоступны. Их
опрашивает Prometheus, а состояние видно на странице `/targets`:

| Компонент | Внутренний адрес | Что отдаёт |
|---|---|---|
| MariaDB | `mariadb:3306` | БД `uptime_kuma` (только для Uptime Kuma и экспортёра) |
| node-exporter | `node-exporter:9100/metrics` | метрики хоста |
| cAdvisor | `cadvisor:8080/metrics` | метрики контейнеров |
| mysqld-exporter | `mysqld-exporter:9104/metrics` | метрики MariaDB |
| blackbox-exporter | `blackbox-exporter:9115/probe` | HTTP/TCP-пробы |

## Проверка работоспособности

### Автоматически

```bash
dagger call validate
```

Команда проверяет 18 пунктов: здоровье всех 8 контейнеров, что все 10
таргетов Prometheus в состоянии `up`, HTTP-пробы Uptime Kuma и Grafana,
`mysql_up` и TCP-пробу MariaDB, latency Uptime Kuma, наличие метрик cAdvisor,
ответ UI Uptime Kuma, health data source Grafana и наличие дашборда. При любой
ошибке команда выходит с ненулевым кодом и показывает строки `[FAIL]` с
причиной. Так её можно использовать в скриптах и CI.

### Вручную

**1. Контейнеры.** У всех 8 должен быть статус `Up … (healthy)`:

```bash
docker ps --filter label=com.docker.compose.project --format 'table {{.Label "com.docker.compose.project"}}\t{{.Label "com.docker.compose.service"}}\t{{.Status}}'
```

**2. Uptime Kuma** отвечает и работает именно на MariaDB, а не на встроенной SQLite:

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:3001/          # 302 → /setup или /dashboard
docker logs uptime-app-uptime-kuma-1 2>&1 | grep -E 'Database Type|Connected to the database'
```

**3. MariaDB** жива, и в ней есть схема Uptime Kuma:

```bash
docker exec uptime-app-mariadb-1 healthcheck.sh --connect --innodb_initialized && echo "MariaDB OK"
docker exec uptime-app-mariadb-1 sh -c 'MYSQL_PWD="$(cat /run/secrets/mariadb_root_password)" mariadb -uroot -e "SHOW TABLES FROM uptime_kuma"'
```

**4. Prometheus** готов, и все таргеты в состоянии `up`:

```bash
curl -fsS http://localhost:9090/-/ready
curl -fsS http://localhost:9090/api/v1/targets | jq -r '.data.activeTargets[] | "\(.health)\t\(.scrapePool)\t\(.labels.instance)"'
```

**5. Ключевые метрики.** Доступность (`1` = UP), latency и CPU контейнеров:

```bash
# статус проб: uptime-kuma, grafana, prometheus (HTTP) и mariadb (TCP)
curl -fsS --get http://localhost:9090/api/v1/query --data-urlencode 'query=probe_success' | jq -r '.data.result[] | "\(.metric.service)\t\(.value[1])"'
# MariaDB глазами mysqld-exporter
curl -fsS --get http://localhost:9090/api/v1/query --data-urlencode 'query=mysql_up' | jq -r '.data.result[].value[1]'
# время ответа веб-сервисов, секунды
curl -fsS --get http://localhost:9090/api/v1/query --data-urlencode 'query=probe_duration_seconds{job="blackbox-http"}' | jq -r '.data.result[] | "\(.metric.service)\t\(.value[1])"'
# CPU контейнеров платформы, % от одного ядра
curl -fsS --get http://localhost:9090/api/v1/query --data-urlencode 'query=sum by (name) (rate(container_cpu_usage_seconds_total{container_label_com_docker_compose_project=~"uptime-.*"}[1m])) * 100' | jq -r '.data.result[] | "\(.metric.name)\t\(.value[1])"'
# сработавшие алерты (0, если всё в порядке)
curl -fsS http://localhost:9090/api/v1/alerts | jq '.data.alerts | length'
```

**6. Grafana** жива, data source отвечает, дашборд на месте:

```bash
curl -fsS http://localhost:3000/api/health
curl -fsS -u "admin:$(cat .secrets/grafana_admin_password)" http://localhost:3000/api/datasources/uid/prometheus/health | jq -r .status    # OK
curl -fsS -u "admin:$(cat .secrets/grafana_admin_password)" 'http://localhost:3000/api/search?query=Platform' | jq -r '.[] | "\(.title)\t\(.url)"'
```

Визуально: откройте дашборд *Platform Health*. В блоке **Overview** у
*Uptime Kuma* и *MariaDB* должно быть зелёное `UP`, а *Scrape targets up*
должно показывать `100%`.

**7. Порты слушают только loopback:**

```bash
ss -ltn | grep -E ':(3000|3001|9090)\b'   # везде 127.0.0.1
```

### Проверка сбоя

Чтобы убедиться, что мониторинг замечает падение, остановите Uptime Kuma:

```bash
docker stop uptime-app-uptime-kuma-1
dagger call validate        # выйдет с ошибкой: [FAIL] container/uptime-kuma, uptime-kuma/http-probe, uptime-kuma/http
docker start uptime-app-uptime-kuma-1
```

На дашборде статус *Uptime Kuma* сменится на красный `DOWN`, а на таймлайне
*Probe status* появится красный участок. Через минуту в Prometheus сработает
алерт `UptimeKumaDown`.

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
