# Тестирование: от валидации до нагрузки

Все команды выполняются из корня распакованного проекта. Итоги проверок, выполненных при подготовке архива, отдельно перечислены в `VERIFICATION.md`. Наличие теста в архиве и успешное выполнение этого теста — разные утверждения.

## 1. Быстрые проверки без PostgreSQL

```powershell
go mod verify
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./...
go build -o bin/api ./cmd/api
go build -o bin/admin ./cmd/admin
```

Для `-race` нужен поддерживаемый Go race detector и C toolchain. На Windows установи подходящий GCC/MinGW либо запускай race-проверку в Linux/CI. Обычные unit-тесты запускаются без Docker.

Пакеты проверяют бизнес-валидацию, месяцы, переполнение/границы, подпись и claims JWT, strict JSON/query, If-Match, cursor и его привязку к владельцу/фильтру, rendezvous и свойства перемещения ключей. HTTP-тесты используют тестовую реализацию store; настоящие SQL и блокировки они не проверяют.

```powershell
go test -run '^$' -bench . -benchmem ./internal/hashring
go test -fuzz FuzzMonthRoundTrip -fuzztime 10s ./internal/domain
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

`govulncheck` использует текущую базу известных уязвимостей. Успех не доказывает отсутствие собственных логических ошибок. Benchmark измеряет только вычисление маршрута уже созданным ring, без HTTP, JWT и БД.

## 2. Интеграционные тесты: три независимых PostgreSQL

В `docker-compose.test.yml` отдельные контейнеры и порты 15433–15435. В них базы `subscriptions_test`, временные tmpfs-хранилища и публичные тестовые пароли. Эти пароли разрешены только в этом локальном стенде; рабочий Compose генерирует случайные секреты.

PowerShell:

```powershell
docker compose -f docker-compose.test.yml -p subscriptions-tests up -d --wait
. ./tests/integration/env.ps1
go test -race -tags=integration -count=1 -v ./tests/integration
docker compose -f docker-compose.test.yml -p subscriptions-tests down
```

Linux/macOS:

```sh
docker compose -f docker-compose.test.yml -p subscriptions-tests up -d --wait
. ./tests/integration/env.sh
go test -race -tags=integration -count=1 -v ./tests/integration
docker compose -f docker-compose.test.yml -p subscriptions-tests down
```

Этот тест действительно удаляет и пересоздаёт таблицы в трёх тестовых базах. Защиты: `TEST_ALLOW_RESET=yes`, явные шесть DSN, обязательное имя базы `subscriptions_test`. Не направляй эти переменные на рабочие данные. Имя базы — дополнительная защита от ошибки, а не способ распознать production по содержимому.

Что проверяется:

1. Применение SQL-миграции и безопасный повтор.
2. CRUD, keyset pagination, помесячная сумма.
3. Повтор POST и конфликт при другом теле с тем же ключом.
4. Одновременные POST с одним ключом возвращают один ID.
5. Устаревший If-Match блокирует PATCH/DELETE.
6. RLS скрывает строки без tenant context и с чужим tenant context; app не может менять routing guard.
7. Выбор пользователя, который действительно перемещается при добавлении шарда.
8. Freeze блокирует старый runtime; ошибочная идентичность шарда отклоняется.
9. Copy можно повторить до activation; данные, счётчик пользователя и idempotency переносятся вместе.
10. Activate можно повторить; старый fingerprint больше не обслуживает запросы.
11. После acknowledge можно удалить устаревшие копии через gc-stale.

Один сценарий не заменяет crash/fault-injection кампанию. Перед важными данными отдельно проверь разрыв соединения на каждой фазе, перезапуск PostgreSQL, нехватку диска, таймаут, восстановление backup и длительное копирование на объёме, близком к рабочему.

## 3. Ручная проверка HTTP

Запусти сервис по README и выполни его CRUD-примеры. Затем проверь негативные случаи:

| Действие | Ожидаемый результат |
|---|---|
| Запрос без Bearer JWT / с просроченным JWT | 401 |
| POST с токеном только `subscriptions:read` | 403 |
| GET чужого UUID подписки | 404 |
| POST с `user_id`, неизвестным или повторённым JSON-полем | 400 |
| POST без Idempotency-Key | 400 |
| Повтор POST с прежними ключом и телом | тот же ID |
| Прежний ключ и другая цена | 409 |
| PATCH/DELETE без If-Match | 428 |
| PATCH/DELETE с устаревшей версией | 412 |
| end_date раньше start_date | 400 |
| Список с чужим/изменённым/просроченным cursor | 400 |
| Перегрузка лимитера или очереди | 429 либо 503 по причине |
| Недоступный шард владельца / замороженная топология | 503 |

Проверять отсутствие утечки можно двумя токенами из README. Административные роли в таких проверках не используют: они специально имеют расширенные права.

## 4. Наполнение и k6

Запусти локальный сервис, затем создай 1000 новых пользователей по 10 подписок:

```powershell
go run ./cmd/seed -users 1000 -per-user 10 -out .local/tokens.json
k6 run tests/load/k6.js
```

Установку k6 делай по официальной инструкции: https://grafana.com/docs/k6/latest/set-up/install-k6/ . Скрипт читает `.local/tokens.json` относительно своего расположения. Для другого файла:

```powershell
k6 run -e TOKENS_FILE=../../.local/tokens-second.json -e BASE_URL=http://127.0.0.1:8080 tests/load/k6.js
```

`seed` принимает только явный loopback HTTP-адрес, подписывает локальные JWT на один час и пишет новый файл с ограниченными правами. Он не перезаписывает файл токенов. При ошибке наполнение может быть частичным, а выходной файл — пустым: сначала выясни причину, затем используй новое имя файла и, при необходимости, пересоздай только тестовые данные. Не запускай генератор с production-ключом.

k6 постепенно поднимает 100 VU, чередует пользователей, читает список и каждый десятый цикл считает сумму. Пороги p95 < 200 ms, p99 < 500 ms и errors < 1% — заданные цели для эксперимента, а не измеренные характеристики проекта. Это read-heavy smoke workload; он не моделирует все реальные записи и конфликты.

Для стенда с настоящим IdP получай токены тестовых пользователей через IdP, без локального private.pem. Не передавай JWT в отчёты, issue или Git.

## 5. Как проверять масштаб

Сначала измерь ожидаемый профиль: активные пользователи, запросы в час, пики, доли CRUD/total, число подписок, распределение арендаторов. Например, 1 млн пользователей × 10 подписок = 10 млн строк, но вовсе не 1 млн одновременных запросов.

Нужны как минимум:

- Warm-up, затем 30–60 минут устойчивой нагрузки и отдельный burst-тест.
- Несколько долей записей и total-запросов; минимум тысячи разных владельцев.
- Тест горячего пользователя отдельно от равномерного распределения.
- Нагрузка на реальных объёмах индексов и диска; миллион пустых аккаунтов бесполезен для SQL capacity.
- p50/p95/p99, RPS, 429/5xx, CPU/RAM, pgx pool wait, PostgreSQL locks, WAL, IO, vacuum, lag реплик.
- Потеря одного шарда: его пользователи получают 503, другие продолжают обслуживаться.
- Масштабирование API-подов и проверка лимита соединений PostgreSQL.
- Restore rehearsal с подтверждёнными RPO/RTO и resharding rehearsal с измеренным окном недоступности.

HPA по CPU — начальный пример. Когда ограничение находится в PostgreSQL, увеличение числа API-подов способно ухудшить ситуацию. Верхний предел пулов планируется вместе с DBA.

## 6. CI и контейнеры

`.github/workflows/ci.yml` содержит unit/race/vet/govulncheck, отдельные PostgreSQL services, интеграционный сценарий и сборку двух Docker targets. GitHub Actions закреплены commit SHA. Workflow добавлен, но его запуск на твоём GitHub здесь не выполнялся.

```sh
docker build --target runtime -t subscriptions:local .
docker build --target admin -t subscriptions-admin:local .
sh scripts/image-check.sh subscriptions:local
sh scripts/image-check.sh subscriptions-admin:local
```

Последний скрипт требует установленные Trivy и Syft, проверяет образ и создаёт SBOM. Release должен дополнительно включать registry scanning, утверждённый порог уязвимостей и подпись образа в вашей инфраструктуре. Сборка сама по себе не запускает эти инструменты.

## Типовые проблемы

- `connection refused`: проверь `docker compose ps -a`, логи БД, порты и правильный config (local использует localhost, docker — имена контейнеров).
- `password authentication failed` после изменения `.env`: существующая PostgreSQL-роль сохранила прежний пароль. Используй штатную ротацию, не удаляй рабочие тома.
- `no healthy shard`: проверь migrate/init, fingerprint, app-role и DSN. Не исправляй проблему ручным обнулением routing_guard.
- 401: токен истёк; issuer/audience/kid не совпадают; часы расходятся; используется не тот public key.
- 503 после изменения списка шардов: простая замена config запрещена протоколом. Нужны фазы переноса из `RESHARDING_RU.md`.
- 429: превышен локальный rate limit; 503 может означать исчерпанную concurrency, timeout, БД или topology fence. Смотри код ошибки/метрики/структурированные логи.
- `seed` вернул 503: проверь готовность БД, ограничения пула и наличие ошибок. Частично созданные данные уже могли сохраниться.
