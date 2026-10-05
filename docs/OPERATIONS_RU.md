# Production: конфигурация, запуск и обслуживание

Архив содержит исполняемый сервис и шаблоны эксплуатации. Домен, IdP, registry, адреса PostgreSQL, CA, приватные сети, секреты и целевые SLO зависят от вашей инфраструктуры. Поэтому значения `example.com`, `REPLACE_WITH_RELEASE_TAG`, issuer/kid и CIDR — обязательные точки настройки, а не готовый публичный deployment.

## 1. Минимальная целевая инфраструктура

- HTTPS ingress/gateway с ограничением размера тела, квотами на пользователя/клиента и защитой от перегрузки.
- Несколько stateless API-подов с одинаковыми config, JWT public keys и CURSOR_KEY.
- Каждый логический шард — отдельный PostgreSQL primary с собственными HA-репликами, backups и WAL/PITR по требованиям RPO/RTO. Шард A не является резервной копией B.
- Внешний Identity Provider, выпускающий RS256 JWT: UUID `sub`, ожидаемые `iss`, `aud`, `kid`, `iat`, `exp` и scopes. API не реализует регистрацию/пароли пользователей.
- Secret manager либо управляемые Kubernetes Secrets с ограниченным RBAC и шифрованием at rest.
- Сбор JSON-логов, Prometheus, Grafana и доставка alerts ответственным людям. Файлы alerts не отправляют уведомления сами; нужен настроенный Alertmanager/эквивалент.

Для межзонного/межрегионального размещения заранее измерь latency транзакций. В проекте нет глобальных распределённых транзакций, multi-region active-active и глобального каталога всех подписок.

## 2. Артефакты релиза

```sh
go mod verify
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
docker build --target runtime -t YOUR_REGISTRY/subscriptions:YOUR_TAG .
docker build --target admin -t YOUR_REGISTRY/subscriptions-admin:YOUR_TAG .
sh scripts/image-check.sh YOUR_REGISTRY/subscriptions:YOUR_TAG
sh scripts/image-check.sh YOUR_REGISTRY/subscriptions-admin:YOUR_TAG
```

Перед публикацией образов выполни integration suite. `image-check.sh` использует Trivy и Syft; установи их отдельно и закрепи одобренные версии в release pipeline. Скрипт останавливается на HIGH/CRITICAL и генерирует `sbom.spdx.json`; перенеси/переименуй результат до проверки второго образа, чтобы сохранить оба SBOM.

Runtime/admin собираются отдельно, запускаются пользователем 65532 из scratch-образов без shell. В runtime есть CA bundle, но нет приватного JWT-ключа, миграционного CLI и admin-DSN. CGO отключён. Для воспроизводимого выпуска закрепляй также digest базового Go-образа и digest двух полученных образов: точный тег версии сам по себе ещё не immutable digest.

`.github/workflows/ci.yml` проверяет код, зависимости, три PostgreSQL и сборку. Он не пушит образы, не деплоит и не выдаёт себе secrets. Registry, provenance/signing и release promotion подключаются в вашей инфраструктуре. Dependabot конфигурация добавлена; обновления проходят те же тесты и review.

## 3. Подготовка PostgreSQL

В локальном Compose роли создаёт `deploy/postgres/init.sh`. В production DBA создаёт аналогичные роли штатным способом через secret manager:

- `subscriptions_migrator`: LOGIN, NOSUPERUSER, NOCREATEDB, NOCREATEROLE, BYPASSRLS; USAGE/CREATE на выделенной схеме public; владелец созданных объектов.
- `subscriptions_app`: LOGIN, NOSUPERUSER, NOCREATEDB, NOCREATEROLE, NOBYPASSRLS; права на таблицы/function выдаёт миграция.

Public CREATE на схеме отозван. Не выдавай app роль migrator через membership, права создавать функции/триггеры или DDL. После provisioning проверь реальные role memberships и grants, а не только флаги самой роли. Managed PostgreSQL может ограничивать создание BYPASSRLS: до запуска согласуй поддерживаемую admin-роль с провайдером и проверь полный SQL suite. Просто запускать API суперпользователем нельзя.

Пример **формы**, не рабочий секрет:

```text
postgres://subscriptions_app:URL_ENCODED_PASSWORD@shard-a.db.example.com:5432/subscriptions?sslmode=verify-full&sslrootcert=/etc/subscriptions/db-ca/ca.crt
```

Код в production-профиле требует `sslmode=verify-full`. Hostname должен совпадать с сертификатом. Специальные символы пароля URL-кодируются. Admin DSN имеет тот же режим TLS, но другую роль. Секреты не печатаются в логах и не записываются в config.json.

Миграции выполняются один раз на релиз отдельной admin-командой. Runtime не делает DDL при старте. `001_initial.sql` в этом архиве предназначен для **новой схемы**, не является upgrade-миграцией прежнего проекта. Для старых данных нужен отдельный проверенный ETL: сохранить user UUID, проверить даты/цены, вычислить bucket, распределить строки, заполнить user_usage, сверить количество и суммы. Не подключай новую версию прямо к старой рабочей базе.

## 4. Секреты и config

Config содержит только параметры и имена переменных; секреты читаются из `NAME` либо `NAME_FILE`. Одновременная установка обоих вариантов считается ошибкой.

| Secret | Что содержит |
|---|---|
| `subscriptions-runtime` | SHARD_A_DSN, SHARD_B_DSN, CURSOR_KEY; после расширения и C |
| `subscriptions-admin` | только SHARD_A_ADMIN_DSN, SHARD_B_ADMIN_DSN и нужные следующие шарды |
| `subscriptions-jwt` | публичный ключ `current.pem` и дополнительные public keys при ротации |
| `subscriptions-db-ca` | файл доверенного CA `ca.crt` для PostgreSQL |

CURSOR_KEY — криптографически случайные 32 байта, 64 hex-символа. Все API-реплики должны читать один ключ, иначе страницы будут случайно отвергаться разными подами. Приватный ключ IdP в приложение не передаётся.

Шаблон env-файла для создания runtime Secret выглядит так; заменяй значения локально, не коммить файл:

```dotenv
SHARD_A_DSN=postgres://...?...sslmode=verify-full...
SHARD_B_DSN=postgres://...?...sslmode=verify-full...
CURSOR_KEY=64_HEX_CHARACTERS
```

Используй secret manager/External Secrets, если он уже принят в компании. Для ручного стенда допустимо создать secret из локального файла через `kubectl create secret generic ... --from-env-file=...`; не вставляй пароли в аргументы `--from-literal`, shell history и issue. Файл после безопасного переноса хранится согласно вашим правилам управления секретами.

## 5. Kubernetes deployment

Шаблоны рассчитаны на Kubernetes **1.34+** и cluster networking с поддержкой NetworkPolicy. Нативный lifecycle `sleep` нужен scratch-контейнеру, в котором нет `/bin/sleep`. Нужны metrics-server для HPA, ingress controller, TLS-сертификат; ServiceMonitor требует установленный Prometheus Operator.

Подготовь:

1. `deploy/k8s/config.json`: issuer, audience, kid/public key path, shard IDs, DSN variable names, размеры пулов. Для новой системы epoch=1, buckets=1024.
2. `kustomization.yaml`: registry и immutable release digest/tag. То же сделай в `*.example.yaml` для admin.
3. `networkpolicy.yaml`: реальные приватные адреса PostgreSQL вместо `10.20.0.0/16`, namespace ingress/monitoring и DNS вашего кластера. При NodeLocal DNS нужны его реальные адреса.
4. `ingress.example.yaml`: реальный host, TLS Secret, ingressClass. HTTP-порт 8080 внутри кластера обслуживает доверенный ingress; TLS завершается там. При обязательном шифровании между ingress и pod нужен принятый в инфраструктуре mTLS/service mesh.
5. Все четыре Secret из предыдущего раздела. В базовых YAML нет заполненных секретов.

Предварительный рендер и проверка против вашего API server:

```sh
kubectl kustomize deploy/k8s > /tmp/subscriptions-rendered.yaml
kubectl apply --dry-run=server -f /tmp/subscriptions-rendered.yaml
```

Последовательность первого развёртывания:

1. Provision PostgreSQL и роли; убедись, что к БД доступен нужный private network.
2. Создай namespace (`kubectl apply -f deploy/k8s/namespace.yaml`), Secrets и TLS.
3. Примени базовый kustomize (`kubectl apply -k deploy/k8s`). API пока может ждать/перезапускаться из-за отсутствия схемы — трафик ещё не открывай.
4. Подставь release image и примени `migrate-job.example.yaml`; дождись `kubectl -n subscriptions wait --for=condition=complete job/subscriptions-migrate-001 --timeout=15m`, изучи логи.
5. **Только для новой пустой системы** выполни аналогичный `init-job.example.yaml` и дождись job/subscriptions-init-001. Для изменения числа шардов вместо init требуется протокол из RESHARDING_RU.
6. Дождись `kubectl -n subscriptions rollout status deployment/subscriptions`. Проверь данные, health и метрики внутри сети.
7. Примени подготовленный ingress. После smoke-теста подключай рабочий трафик.
8. Примени `cleanup-cronjob.example.yaml`; при наличии Operator — ServiceMonitor. Проверь, что alerts реально доставляются.

У Job имя содержит номер: перед следующим релизом выбери новое имя. Поля template у существующего Job нельзя произвольно менять. Не запускай init как часть каждого обновления схемы или переноса данных.

Обычное обновление: совместимая миграция → обновление образа/config → rollout → smoke. Миграции не редактируются задним числом; добавляется следующий SQL-файл, его checksum сохраняется. Для несовместимого изменения схемы нужен expand/contract и отдельный порядок релизов. Универсального down-migration нет: откат приложения допускается только при совместимой схеме.

ConfigMap имеет стабильное имя, поэтому изменение содержимого само не создаёт новые поды. После изменения config/public keys явно делай rollout restart. **Изменение топологии не является обычным config reload**: сначала resharding.

## 6. Health, HPA и соединения

`/livez` показывает работоспособность процесса, а `/readyz` — что процесс не draining и есть хотя бы один доступный шард с нужным fingerprint. Это осознанно сохраняет обслуживание остальных пользователей при отказе одного шарда. `/metrics` содержит отдельную `subscriptions_shard_ready` для каждого шарда; одного зелёного readyz недостаточно, чтобы объявить все данные доступными.

Метрики не включают user_id, subscription_id, произвольный URL или JWT в labels. Иначе число временных рядов росло бы с числом пользователей. Диагностический порт 9090 не публикуется ingress.

При 10 API-подах и pool_max=20 **каждый шард** может получить 200 соединений от приложения, плюс surge pod, health/admin/операторы и сторонние клиенты. Общая верхняя граница API-соединений при 3 шардах — 600. Учти это в PostgreSQL max_connections/ресурсах и HPA; пул не обязан немедленно открыть максимум.

PDB защищает от части добровольных disruption, но не от всех отказов и не заменяет HA PostgreSQL. Deployment распределяет поды по hostname; размещение БД по зонам и failure domains выполняется отдельно. Перед добавлением PgBouncer transaction pooling проверь prepared statements и весь integration suite в вашей версии/configuration; sidecar proxy здесь не включён.

## 7. Backup, restore и потеря шарда

`scripts/backup.sh` создаёт логический custom-format pg_dump одного шарда. Задай PGHOST/PGPORT/PGDATABASE/PGUSER/PGSSLMODE/PGSSLROOTCERT и PGPASSFILE с правами 0600, `BACKUP_FILE` — новый путь. Роль backup должна видеть все строки (обычно отдельная согласованная роль с BYPASSRLS). Скрипт отказывается работать на frozen/pending топологии и проверяет fingerprint до/после.

```sh
export PGHOST=shard-a.db.example.com PGPORT=5432 PGDATABASE=subscriptions
export PGUSER=subscriptions_migrator PGSSLMODE=verify-full
export PGSSLROOTCERT=/secure/ca.crt PGPASSFILE=/secure/pgpass
export BACKUP_FILE=/secure/backups/shard-a-2026-10-05.dump
sh scripts/backup.sh
```

Повтори для всех шардов и сохрани рядом версию приложения, schema migrations и точный config/fingerprint. Архив содержит персональные данные: шифруй его средствами backup-хранилища, ограничь доступ и назначь retention. Скрипт сам не реализует encryption, offsite upload, расписание и WAL/PITR. Для production это конфигурация managed PostgreSQL или отдельного проверенного backup-сервиса.

`pg_restore --list` проверяет читаемость каталога архива, но не доказывает восстановимость всех данных. Регулярная репетиция обязательна:

1. Создай **новую изолированную** БД и роли с нужными именами.
2. Установи PG* на неё, BACKUP_FILE на нужный архив, CONFIRM_RESTORE на точное имя target БД.
3. Запусти `sh scripts/restore.sh` от роли, которой должны принадлежать восстановленные объекты, обычно migrator. Не накатывай сначала миграции: pg_dump уже содержит схему.
4. Проверь migrations checksum, guard identity, counts, user_usage и total на контрольных пользователях.
5. Измерь длительность, сравни с RTO и допустимой потерей данных/RPO. Не открывай restored-копию как второй writable primary.

Отказ одного шарда восстанавливается внутри этого логического shard ID: HA failover на его реплику или restore его данных. Автоматически отправлять пользователей на другой hash owner нельзя. Вернувшийся stale primary должен быть ограждён средствами PostgreSQL/инфраструктуры; routing fingerprint сам по себе не защищает от двух primary одного и того же shard ID.

## 8. Ротация, retention, ограничения

JWT public keys: сначала добавь новый kid/public key во все API-поды, затем переключи IdP на новый private key. Дождись истечения максимального TTL старых токенов (здесь до часа плюс допуск времени) и удали старый public key новым rollout. Мгновенная revocation отдельного JWT не реализована; для неё требуется отдельный продуктовый механизм.

CURSOR_KEY: новая версия ключа делает старые cursors недействительными, клиент начинает список заново. Во время обычного rollout с разными cursor keys часть продолжений будет возвращать 400; планируй согласованное переключение или отдельно реализуй key ring, если нужен непрерывный переход.

DSN: ротацию выполняют через смену роли/пароля по правилам PostgreSQL и rollout всех потребителей. Простого изменения значения Secret недостаточно для уже созданных соединений. Не используй admin credentials в API.

Cleanup удаляет истёкшие idempotency records ограниченными партиями. Запускай каждые 5 минут и следи за количеством/возрастом строк; при большом write rate этого может быть недостаточно. При resharding приостанови CronJob, после acknowledge возобнови. 24 часа — минимальный срок повторного воспроизведения операции, затем ключ может быть использован заново после cleanup. Повтор POST после удаления подписки до истечения ключа возвращает сохранённый ответ той первоначальной операции, а не создаёт подписку снова.

Цена — целое число в одной валюте, история тарифов не хранится. Изменение price меняет расчёт за все месяцы этой записи. Для финансового биллинга нужны money/currency, ledger, тарифные интервалы, проводки, аудит и отдельная модель; текущий сервис ведёт пользовательский список подписок.

Удаление аккаунта, глобальная аналитика/CDC, online resharding, привязка внешнего non-UUID subject, weighted shards, кэширование, distributed quotas и S3/WAL automation не реализованы. Они требуют конкретных требований. Не добавляй Redis/Kafka только ради списка технологий: сначала определи задачу и измерь текущий bottleneck.

## 9. Первичные источники

Сведения о механизмах проверялись по официальной документации; за текущими версиями и изменениями следи перед релизом:

- Go releases/security: https://go.dev/doc/devel/release и https://go.dev/security/
- PostgreSQL RLS: https://www.postgresql.org/docs/16/ddl-rowsecurity.html
- PostgreSQL locks: https://www.postgresql.org/docs/16/explicit-locking.html
- PostgreSQL TLS: https://www.postgresql.org/docs/16/libpq-ssl.html
- Backup/restore: https://www.postgresql.org/docs/16/backup.html
- pgx: https://pkg.go.dev/github.com/jackc/pgx/v5
- JWT validator: https://pkg.go.dev/github.com/golang-jwt/jwt/v5
- Prometheus labels: https://prometheus.io/docs/practices/naming/
- Kubernetes probes: https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/
- Lifecycle hooks: https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/
- NetworkPolicy: https://kubernetes.io/docs/concepts/services-networking/network-policies/
- k6: https://grafana.com/docs/k6/latest/
