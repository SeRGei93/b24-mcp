#!/bin/sh
# Git pre-push hook: перед пушем в main/master прогоняем проверки локально.
# Обойти hook: SKIP_PREPUSH=1 git push
set -eu

cd "$(git rev-parse --show-toplevel)"

if [ "${SKIP_PREPUSH:-}" = "1" ]; then
  echo "pre-push: проверки пропущены (SKIP_PREPUSH=1)"
  exit 0
fi

pushes_to_main=0
while read -r _local_ref _local_sha remote_ref _remote_sha; do
  case "$remote_ref" in
    refs/heads/master | refs/heads/main) pushes_to_main=1 ;;
  esac
done

if [ "$pushes_to_main" -eq 0 ]; then
  exit 0
fi

echo "pre-push: пуш в main — прогоняем проверки локально (go vet, go test, go build)"
go vet ./...
go test ./...
CGO_ENABLED=0 go build -o /dev/null ./cmd/b24
