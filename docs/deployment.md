# Деплой, Docker, Dokploy и разработка

В этом разделе описаны способы сборки и развёртывания **Битрикс 24 MCP** локально, в Docker Compose и в PaaS-панели **Dokploy** с автоматическим CI/CD через GitHub Actions, а также процесс локальной разработки и тестирования.

---

## 1. Сборка и запуск бинарника (без Docker)

Проект использует чистый Go (`CGO_ENABLED=0`), поэтому собранный бинарный файл статически слинкован и может запускаться на любом минимальном Linux/macOS/Windows окружении:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o b24 ./cmd/b24
./b24
```

При получении сигналов `SIGINT` или `SIGTERM` сервер корректно завершает текущие HTTP-соединения с таймаутом 10 секунд (`httpServer.Shutdown`) и закрывает базу данных SQLite.

---

## 2. Локальный запуск в Docker Compose

В репозитории предусмотрены два файла для стандартного Docker Compose:
- [`Dockerfile`](../Dockerfile) — двухступенчатая сборка (`golang:1.26-alpine` → `FROM scratch` с одним статическим бинарником `/app/b24`), работающая от непривилегированного пользователя (`1000:1000`) со встроенным `HEALTHCHECK` (`["/app/b24", "-healthcheck"]`).
- [`docker-compose.yml`](../docker-compose.yml) — базовое описание сервиса `app` и тома `sqlite_data:/app/data`.
- [`docker-compose.override.yml`](../docker-compose.override.yml) — автоматически подхватывается при локальном вызове `docker compose up` и публикует порт `${APP_PORT:-3000}:3000` на хост-машину.

Запуск:

```bash
cp .env.example .env
# Заполните обязательные переменные в .env
docker compose up --build -d
```

Просмотр логов и статуса:

```bash
docker compose ps
docker compose logs -f app
```

---

## 3. Продакшн-деплой в Dokploy

Для продакшн-окружения под управлением **Dokploy** подготовлены [`docker-compose.dokploy.yml`](../docker-compose.dokploy.yml) и workflow [`.github/workflows/deploy.yml`](../.github/workflows/deploy.yml).

### Особенности `docker-compose.dokploy.yml`:
- Использует готовый образ `ghcr.io/sergei93/b24-mcp:${IMAGE_TAG:-latest}` с `pull_policy: always` — продакшн-сервер не тратит ресурсы на компиляцию Go.
- Подключается к внешней сети `dokploy-network`, через которую Traefik (встроенный прокси Dokploy) маршрутизирует HTTPS-трафик на порт `3000` контейнера.
- Включает усиление безопасности контейнера (`x-app-hardening`):
  - `security_opt: ["no-new-privileges:true"]`
  - `cap_drop: ["ALL"]`
- Настраивает ротацию логов Docker (`max-size: "20m"`, `max-file: "5"`).
- Хранит файл SQLite `/app/data/bitrix24-mcp.sqlite` в постоянном томе `sqlite_data`.

### Пошаговая настройка в Dokploy и GitHub

1. **Подключение GitHub Container Registry (GHCR) в Dokploy**:
   - В панели Dokploy откройте **Settings → Registry**.
   - Добавьте реестр `ghcr.io` с вашим GitHub-логином и Personal Access Token (PAT) с правом `read:packages`.

2. **Создание Compose-сервиса в Dokploy**:
   - Нажмите **Create → Compose**, выберите ваш GitHub-репозиторий.
   - В поле **Compose Path** укажите:
     ```text
     ./docker-compose.dokploy.yml
     ```
   - Оставьте **Autodeploy** включённым, а в поле **Watch Paths** добавьте `docker-compose.dokploy.yml` (чтобы деплой запускался по вебхуку из GitHub Actions строго *после* завершения сборки образа).
   - Скопируйте **Webhook URL** в настройках Compose-сервиса и добавьте его в секреты GitHub-репозитория (`Settings → Secrets and variables → Actions`) под именем:
     ```text
     DOKPLOY_DEPLOY_WEBHOOK
     ```

3. **Переменные окружения и домен в Dokploy**:
   - На вкладке **Environment** задайте обязательные переменные:
     ```dotenv
     APP_URL=https://mcp.example.com
     BITRIX_CLIENT_ID=local.xxxx.xxxx
     BITRIX_CLIENT_SECRET=xxxx
     BITRIX_PORTAL_DOMAIN=company.bitrix24.ru
     BITRIX_MCP_DEFAULT_MODE=read_only
     MCP_AUTH_TOKEN=<минимум_32_символа>
     TOKEN_ENCRYPTION_KEY=<32_байта_base64_или_64_hex>
     ```
   - На вкладке **Domains** привяжите ваш HTTPS-домен (например, `mcp.example.com`) к сервису `app` на порт `3000`.

4. **Как работает CI/CD (`.github/workflows/deploy.yml`)**:
   - При пуше в ветку `main` или `master` (если изменились файлы в `cmd/**`, `internal/**`, `go.mod`, `go.sum`, `Dockerfile`, `docker-compose.dokploy.yml` или сам workflow) сначала запускается job `test` (`go vet ./...`, `go test -race ./...` и контрольная статическая сборка).
   - После успешного прохождения тестов запускается job `build`, который собирает Docker-образ и пушит теги `ghcr.io/sergei93/b24-mcp:<sha>` и `ghcr.io/sergei93/b24-mcp:latest`.
   - После успешного пуша запускается job `deploy`, который вызывает `DOKPLOY_DEPLOY_WEBHOOK`, и Dokploy обновляет контейнер из свежего образа.

---

## 4. Резервное копирование базы данных SQLite

Все данные сервера (зашифрованные подключения Битрикс24, зарегистрированные OAuth-клиенты, выданные токены и журнал аудита) находятся в одном файле SQLite (и его WAL-файлах при активной записи).

Так как runtime-образ контейнера `app` собран на базе `FROM scratch` и не содержит `/bin/sh`, для горячего консистентного бэкапа из тома `sqlite_data` с учётом WAL используйте одноразовый вспомогательный контейнер с `sqlite3`:

```bash
docker run --rm \
  --volumes-from "$(docker compose ps -q app)" \
  -v "$(pwd):/backup" \
  alpine:3.21 sh -c 'apk add --no-cache sqlite >/dev/null && sqlite3 /app/data/bitrix24-mcp.sqlite ".backup /backup/bitrix24-mcp-backup.sqlite"'
```

Либо при остановленном контейнере скопируйте файл напрямую с помощью `docker compose cp`:

```bash
docker compose cp app:/app/data/bitrix24-mcp.sqlite ./bitrix24-mcp-backup.sqlite
```

> Не забудьте также надёжно сохранить значение `TOKEN_ENCRYPTION_KEY` в вашем менеджере секретов — без него расшифровать токены из бэкапа невозможно.

---

## 5. Локальная разработка и тестирование

### Запуск тестов и статического анализа

Все пакеты покрыты модульными и интеграционными тестами (`config_test.go`, `store_test.go`, `bitrix_test.go`, `mcp_test.go`, `server_test.go`), использующими изолированную SQLite в памяти (`:memory:`) и `httptest`-серверы:

```bash
# Статический анализ
go vet ./...

# Запуск всех тестов
go test -v ./...

# Проверка сборки с отключённым CGO
CGO_ENABLED=0 go build -o /dev/null ./cmd/b24
```

### Git-хуки (`pre-push`)

В репозитории есть скрипт установки git-хука `pre-push`, который автоматически запускает `go vet ./...`, `go test ./...` и контрольную сборку перед каждым пушем в `main` / `master`:

```bash
sh scripts/install-hooks.sh
```

Если в исключительном случае нужно пропустить проверку при пуше:

```bash
SKIP_PREPUSH=1 git push
```
