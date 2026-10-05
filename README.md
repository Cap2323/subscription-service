# Subscriptions Platform

Новая реализация на Go 1.27.1: личные подписки пользователей, JWT, PostgreSQL, шардирование через rendezvous hashing и 1024 логических бакета. Это сервис учёта и агрегации, он не списывает деньги и не хранит историю тарифов.

Проект подготовлен как основа для эксплуатации с большим числом пользователей. Миллион зарегистрированных пользователей не равен миллиону запросов в секунду: реальную ёмкость определяют профиль запросов, число подписок, диски, PostgreSQL и нагрузочные измерения.

## Что внутри

- `cmd/api` — HTTP API и отдельный порт диагностики.
- `cmd/admin` — миграции, инициализация топологии, план и перенос бакетов, очистка.
- `cmd/devtoken` — только локальные ключи и тестовые JWT.
- `cmd/seed` — заполнение локального API тестовыми пользователями.
- `internal/domain` — данные и бизнес-правила.
- `internal/hashring` — rendezvous, вычисление бакета и плана перемещений.
- `internal/store` — пулы соединений, транзакции и SQL.
- `internal/auth` — проверка JWT.
- `internal/httpapi` — маршруты, JSON, ошибки, пагинация, ограничения.
- `internal/topology` — административный протокол переноса данных.
- `migrations` — SQL, встроенный в admin-бинарник, и контрольная сумма миграций.
- `deploy` — Docker/PostgreSQL, Kubernetes, Prometheus, Grafana, alerts.
- `tests` — интеграционная проверка на трёх БД и k6.
- `docs` — подробные объяснения, OpenAPI, эксплуатация и ограничения проверки.

Единое руководство со всеми разделами: `GUIDE_RU.md`. Фактические результаты проверки: `docs/VERIFICATION.md`.

Начни с этого README. Затем читай `docs/ARCHITECTURE_RU.md`, `docs/DATABASE_RU.md`, `docs/CODE_RU.md`, `docs/TESTING_RU.md`, `docs/RESHARDING_RU.md`, `docs/OPERATIONS_RU.md`.

## Запуск на Windows

Нужны Go 1.27.1 и Docker Desktop с Linux containers / Compose. После установки открой новый PowerShell и проверь:

```powershell
go version
docker version
docker compose version
```

Распакуй проект в новую папку, открой терминал именно в папке, где находится `go.mod`. Не накладывай этот архив поверх прежнего проекта: у новой версии другой контракт API и новая схема БД.

Первый запуск:

```powershell
go run ./cmd/devtoken -init
docker compose up --build -d
docker compose ps -a
docker compose logs migrate init-topology app
```

Первая команда создаёт `.env` со случайными паролями и `.local/private.pem`, `.local/public.pem`. Существующие секреты она не перезаписывает. Приватный ключ используется только локальным генератором JWT и не попадает в API-контейнер.

`migrate` и `init-topology` должны завершиться с кодом 0. `app`, `db-a`, `db-b` продолжают работать. PostgreSQL-шарды содержат независимые базы. Они не являются репликами друг друга.

Адреса:

| Назначение | Адрес |
|---|---|
| API | http://127.0.0.1:8080/api/v1/subscriptions |
| Жив ли процесс | http://127.0.0.1:9090/livez |
| Готов ли обслуживать хотя бы один шард | http://127.0.0.1:9090/readyz |
| Метрики | http://127.0.0.1:9090/metrics |
| PostgreSQL A | 127.0.0.1:5433 |
| PostgreSQL B | 127.0.0.1:5434 |

Эти порты Compose публикует только на loopback компьютера. Kubernetes-вариант не публикует базы и диагностический порт наружу.

Проверка здоровья:

```powershell
Invoke-RestMethod http://127.0.0.1:9090/livez
Invoke-RestMethod http://127.0.0.1:9090/readyz
```

## Сделай первый запрос

JWT действует 15 минут. Повтори команду получения токена, если API начал отвечать 401.

```powershell
$token = (go run ./cmd/devtoken).Trim()
$headers = @{
    Authorization = "Bearer $token"
    "Idempotency-Key" = [guid]::NewGuid().ToString()
}
$body = @{
    service_name = "Yandex Plus"
    price = 400
    start_date = "07-2025"
} | ConvertTo-Json
$sub = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/subscriptions" -Headers $headers -ContentType "application/json; charset=utf-8" -Body $body
$sub
```

`user_id` в тело запроса передавать не нужно: сервер берёт его из подписанного JWT. В тестовом токене по умолчанию используется `60601fee-2bf1-4721-ae6f-7636e79a0cba`.

Повтори POST с теми же `$headers` и `$body`: должен вернуться тот же `id`, без новой записи. Для другой операции создай новый `Idempotency-Key`. Изменение тела с прежним ключом вернёт 409.

Получение и список:

```powershell
$id = $sub.id
Invoke-RestMethod -Uri "http://127.0.0.1:8080/api/v1/subscriptions/$id" -Headers $headers
Invoke-RestMethod -Uri "http://127.0.0.1:8080/api/v1/subscriptions?limit=20" -Headers $headers
Invoke-RestMethod -Uri "http://127.0.0.1:8080/api/v1/subscriptions/total?from=07-2025&to=09-2025" -Headers $headers
```

Для единственной подписки с ценой 400 результат суммы — 1200. Обе границы периода включаются. Цена измеряется в целых денежных единицах одной выбранной для сервиса валюты; смешивать разные валюты в этом поле нельзя.

Частичное обновление:

```powershell
$updateHeaders = @{
    Authorization = "Bearer $token"
    "If-Match" = '"' + $sub.version + '"'
}
$patch = @{ price = 500; end_date = "08-2025" } | ConvertTo-Json
$sub = Invoke-RestMethod -Method Patch -Uri "http://127.0.0.1:8080/api/v1/subscriptions/$id" -Headers $updateHeaders -ContentType "application/json; charset=utf-8" -Body $patch
$sub
```

Версия увеличится с 1 до 2. Попытка обновить ещё раз со старым `If-Match: "1"` вернёт 412. Нужно перечитать запись, решить конфликт, затем отправить новую версию. Просто слепо повторять конфликтующее изменение нельзя.

Чтобы сделать подписку бессрочной:

```powershell
$updateHeaders["If-Match"] = '"' + $sub.version + '"'
$sub = Invoke-RestMethod -Method Patch -Uri "http://127.0.0.1:8080/api/v1/subscriptions/$id" -Headers $updateHeaders -ContentType "application/json" -Body '{"end_date":null}'
```

Удаление:

```powershell
$updateHeaders["If-Match"] = '"' + $sub.version + '"'
Invoke-RestMethod -Method Delete -Uri "http://127.0.0.1:8080/api/v1/subscriptions/$id" -Headers $updateHeaders
```

Правильный ответ — 204 без JSON-тела. Если другой пользователь попробует получить этот ID, API вернёт 404.

## Второй пользователь

```powershell
$otherToken = (go run ./cmd/devtoken -user "00000000-0000-4000-8000-000000000002").Trim()
Invoke-RestMethod -Uri "http://127.0.0.1:8080/api/v1/subscriptions" -Headers @{Authorization="Bearer $otherToken"}
```

Он увидит только свой список. Строка issuer `https://issuer.local.test` — проверяемое значение JWT, а не адрес сервера, который нужно запускать для локальной разработки.

## Локальный Go-процесс вместо API-контейнера

Сначала подготовь `.env` и ключи, затем:

```powershell
docker compose up -d db-a db-b
. ./scripts/env.ps1
go run ./cmd/admin -config configs/local.json -command migrate
go run ./cmd/admin -config configs/local.json -command init -execute
go run ./cmd/api -config configs/local.json
```

Не запускай одновременно локальный API и контейнер app на порту 8080. Если контейнер уже работает, сначала `docker compose stop app`. На Linux/macOS переменные загружаются `. ./scripts/env.sh` из корня проекта.

## Метрики и Grafana

```powershell
docker compose --profile monitoring up -d prometheus grafana
```

Prometheus: http://127.0.0.1:9091. Grafana: http://127.0.0.1:3000, логин `admin`, пароль — значение `GRAFANA_PASSWORD` из твоего `.env`. Он отдельный от паролей БД. Dashboard Subscriptions API загружается автоматически.

OpenAPI находится в `docs/openapi.yaml`; его можно импортировать в инструмент работы с OpenAPI. Публичный Swagger UI в production API намеренно не включён.

## Остановка и данные

```powershell
docker compose down
```

Тома с базами сохраняются. Команда `down -v` удаляет данные и не нужна для обычного обновления. Изменение пароля в `.env` не меняет пароль уже созданной PostgreSQL-роли: init-скрипт выполняется только на пустом томе.

Секреты, JWT, приватные ключи и бэкапы не должны попадать в Git. В `.gitignore` уже добавлены соответствующие пути.
