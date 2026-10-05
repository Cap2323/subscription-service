# Dot-source from project root after starting docker-compose.test.yml.
$env:TEST_ALLOW_RESET = "yes"
$port = 15433
foreach ($letter in @("A", "B", "C")) {
    [Environment]::SetEnvironmentVariable("TEST_SHARD_${letter}_DSN", "postgres://subscriptions_app:local-test-app@127.0.0.1:${port}/subscriptions_test?sslmode=disable", "Process")
    [Environment]::SetEnvironmentVariable("TEST_SHARD_${letter}_ADMIN_DSN", "postgres://subscriptions_migrator:local-test-migrator@127.0.0.1:${port}/subscriptions_test?sslmode=disable", "Process")
    $port++
}
