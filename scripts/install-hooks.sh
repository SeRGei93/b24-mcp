#!/bin/sh
# Ставит git-хук pre-push, который вызывает scripts/pre-push.sh.
# Установка: sh scripts/install-hooks.sh
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"

if [ ! -e "$root/.git" ]; then
  exit 0
fi

hooks_dir="$root/.git/hooks"
mkdir -p "$hooks_dir"

cat > "$hooks_dir/pre-push" <<'HOOK'
#!/bin/sh
# Сгенерировано scripts/install-hooks.sh. Правьте scripts/pre-push.sh.
exec sh "$(git rev-parse --show-toplevel)/scripts/pre-push.sh" "$@"
HOOK

chmod +x "$hooks_dir/pre-push"
echo "install-hooks: pre-push установлен"
