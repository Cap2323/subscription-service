#!/bin/sh
# Source from project root; isolated test databases only.
export TEST_ALLOW_RESET=yes
export TEST_SHARD_A_DSN='postgres://subscriptions_app:local-test-app@127.0.0.1:15433/subscriptions_test?sslmode=disable'
export TEST_SHARD_A_ADMIN_DSN='postgres://subscriptions_migrator:local-test-migrator@127.0.0.1:15433/subscriptions_test?sslmode=disable'
export TEST_SHARD_B_DSN='postgres://subscriptions_app:local-test-app@127.0.0.1:15434/subscriptions_test?sslmode=disable'
export TEST_SHARD_B_ADMIN_DSN='postgres://subscriptions_migrator:local-test-migrator@127.0.0.1:15434/subscriptions_test?sslmode=disable'
export TEST_SHARD_C_DSN='postgres://subscriptions_app:local-test-app@127.0.0.1:15435/subscriptions_test?sslmode=disable'
export TEST_SHARD_C_ADMIN_DSN='postgres://subscriptions_migrator:local-test-migrator@127.0.0.1:15435/subscriptions_test?sslmode=disable'
