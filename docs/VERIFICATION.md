# Проверки этого архива

Дата: 5 октября 2026. Это отчёт о реально выполненных проверках, а не гарантия отсутствия ошибок или измеренная производительность production.

## Выполнено успешно

| Проверка | Результат |
|---|---|
| Go 1.27.1 / Linux amd64 | использован для проверки и сборки |
| `go mod verify` | all modules verified |
| `gofmt` | исходники отформатированы |
| `go test -race -count=1 ./...` | 14 unit test functions, подслучаи и seed corpus fuzz-теста прошли в 5 пакетах |
| `go vet ./...` | замечаний нет |
| Статическая сборка api/admin с CGO_ENABLED=0 | успешно |
| Сборка devtoken/seed | успешно |
| Генерация локальных секретов и JWT | проверена во временной папке: разные секреты, JWT 15 минут, повтор init не перезаписывает файлы |
| `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` | No vulnerabilities found |
| `go test -tags=integration -run '^$' ./tests/integration` | компиляция успешна; ноль integration tests выполнено |
| YAML/JSON и shell scripts | parsing всех конфигураций и `sh -n` прошли |
| OpenAPI 3.0.3 | проходит openapi-spec-validator |
| Kustomize v5.8.2 build deploy/k8s | рендер успешен |
| SQL smoke на PGlite PostgreSQL WASM | 8 групп проверок прошли |

Среда располагалась внутри внешнего Git-workspace с недоступным VCS metadata для Go build. Для локальной сборки бинарников использован `-buildvcs=false`. В распакованном проекте и обычном корректном Git-checkout такой флаг не требуется; Docker build не включает Git metadata.

Пакеты `internal/store` и `internal/topology` не имеют собственных unit-файлов: их реальные SQL/конкурентные сценарии находятся в integration suite. Строки `[no test files]` не означают полное покрытие этих пакетов. Число unit test functions не является процентом покрытия.

## Проверка распределения и microbenchmark

Тест использует миллион детерминированных UUID и тестовые shard IDs `a`, `b`, `c`:

```text
a: 341178
b: 308701
c: 350121
```

При 1024 бакетах баланс приблизительный. Это проверка распределения ключей; никаких миллиона реальных пользователей/HTTP-сессий/записей в PostgreSQL в этом эксперименте не было. В shipped config IDs называются `shard-a`, `shard-b`, `shard-c`, поэтому их конкретное распределение отличается.

Отдельный пример plan для shipped config 2→3 шарда: **338/1024 бакетов**. Добавление узла не перемещает бакеты между двумя прежними узлами; это свойство покрыто тестом.

Одно измерение route на Intel Xeon Platinum 8573C:

```text
BenchmarkRoute-8  8349952  133.6 ns/op  0 B/op  0 allocs/op
```

Измерялся готовый ring, 8 shard IDs, без инициализации, HTTP, JWT, TLS и БД. Из этого числа нельзя получить честный RPS сервиса. При изменении CPU/Go/набора данных результат меняется.

## Что именно проверил PGlite

Выполнена настоящая SQL-миграция из проекта. SQL для monthly total извлекался из Go-исходника, чтобы не проверять отдельную вручную переписанную формулу.

1. DDL и grants миграции.
2. SECURITY DEFINER routing fence.
3. RLS: owner видит свои строки, чужой пользователь — нет.
4. Transaction-local tenant context сбрасывается после COMMIT.
5. Пересечение месяцев, ожидаемая сумма 1300 на контрольных данных.
6. Имя с `%` обрабатывается как литерал, не wildcard.
7. Frozen topology отклоняется fence-проверкой.
8. JSONB roundtrip полных строк при копировании бакета сохраняет данные.

PGlite — PostgreSQL, собранный для WebAssembly, а не три независимых native PostgreSQL instances. Этот smoke не проверяет сетевой pgx-протокол, конкурентные locks между серверами, Docker entrypoint, HA, durable fsync/PITR или весь admin rebalance end-to-end.

## Не выполнено в среде подготовки

- Native PostgreSQL integration suite: Docker/native PostgreSQL здесь отсутствовали. Для запуска приложены отдельный Compose, env scripts и GitHub Actions.
- Docker build и полный `docker compose up`.
- k6 HTTP/БД-нагрузка, soak, chaos, crash/restart во время переноса.
- Реальное развёртывание Kubernetes, server-side admission/dry-run, probes/HPA/NetworkPolicy в живом кластере.
- Trivy/Syft образов и registry scanning: образы здесь не собирались.
- End-to-end restore из pg_dump и WAL/PITR rehearsal.
- Интеграция с вашим IdP, TLS CA и production secret manager.
- Эксплуатационная готовность для конкретного SLO, RPS, RPO и RTO.

Эти проверки входят в порядок подготовки production-релиза из OPERATIONS_RU и TESTING_RU. Не интерпретируй компиляцию integration suite как подтверждённый перенос реальных данных.
