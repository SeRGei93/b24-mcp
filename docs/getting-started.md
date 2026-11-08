# Быстрый старт и настройка Битрикс24

В этом руководстве описан процесс подготовки локального (или тиражного) приложения в Битрикс24, генерации ключей безопасности, запуска **Битрикс 24 MCP** и подключения MCP-клиентов.

---

## 1. Предварительные требования

- **Go 1.26+** (для сборки из исходников) или **Docker + Docker Compose**.
- Доступ к порталу **Битрикс24** (облачный домен `*.bitrix24.*` или явно разрешённый коробочный домен через `BITRIX_PORTAL_DOMAIN`) с правами на добавление локального приложения.
- Публичный **HTTPS-адрес** для сервера (для продакшна и колбэков Битрикс24) или `http://localhost:3000` для локальной разработки.

---

## 2. Создание локального приложения в Битрикс24

Сервер использует официальный OAuth 2.0 протокол Битрикс24. Чтобы получить `client_id` и `client_secret`:

1. Откройте ваш портал Битрикс24 и перейдите в раздел **Разработчикам** (`Developer resources`) → **Другое** (`Other`) → **Локальное приложение** (`Local application`).
2. Выберите тип приложения: **Серверное** (использует только API, без интерфейса встройки, либо с первоначальной установкой).
3. Укажите пути обработчиков (замените `https://your-server.example.com` на ваш `APP_URL`):
   - **Путь вашего обработчика (`Handler path`)**:
     ```text
     https://your-server.example.com/oauth/bitrix/callback
     ```
   - **Путь для первоначальной установки (`Initial installation path`)**:
     ```text
     https://your-server.example.com/oauth/bitrix/install
     ```
4. В разделе **Права (`Permissions / Scope`)** выберите:
   - `CRM (crm)` — обязательно для работы со сделками, лидами, контактами, компаниями и смарт-процессами.
   - `Пользователи (user)` — рекомендуется для работы инструмента `bitrix_list_users` и проверки профиля авторизованного пользователя.
5. Сохраните приложение и скопируйте выданные **Код приложения (`client_id`)** и **Ключ приложения (`client_secret`)**.

---

## 3. Подготовка конфигурации `.env`

Скопируйте шаблон переменных окружения:

```bash
cp .env.example .env
```

Сгенерируйте два криптографических секрета:

```bash
# 1. Ключ шифрования токенов Битрикс24 в БД (32 байта в base64 или 64 hex-символа)
openssl rand -base64 32

# 2. Статический токен доступа к MCP (минимум 32 символа)
openssl rand -hex 32
```

Заполните `.env`:

```dotenv
APP_URL=https://your-server.example.com
HOST=0.0.0.0
PORT=3000
LOG_LEVEL=info
MCP_TRANSPORT=http

DATABASE_PATH=./data/bitrix24-mcp.sqlite

BITRIX_CLIENT_ID=local.xxxxxxxxxxxxxxxx.xxxxxxxx
BITRIX_CLIENT_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
BITRIX_PORTAL_DOMAIN=your-portal.bitrix24.ru

BITRIX_MCP_DEFAULT_MODE=read_only
BITRIX_MCP_ADMIN_TOOLS=false

TOKEN_ENCRYPTION_KEY=<ваш_ключ_из_openssl_rand_base64_32>
MCP_AUTH_TOKEN=<ваш_токен_из_openssl_rand_hex_32>
```

> **Совет:** Если вам нужны операции создания, редактирования и смены стадий в CRM, установите `BITRIX_MCP_DEFAULT_MODE=write_with_confirmation`. Подробнее см. в разделе [Конфигурация](configuration.md).

---

## 4. Запуск сервера

### Вариант А: Сборка и запуск бинарника Go

Сервер компилируется в статический бинарный файл без зависимостей от C (`CGO_ENABLED=0`):

```bash
CGO_ENABLED=0 go build -o b24 ./cmd/b24
./b24
```

При старте сервер автоматически создаст директорию для SQLite (`./data`) и применит встроенные SQL-миграции.

Проверка работоспособности:

```bash
curl http://localhost:3000/health
# {"database":"connected","status":"ok"}
```

### Вариант Б: Запуск через Docker Compose

```bash
docker compose up --build -d
```

В локальном окружении автоматически подхватывается `docker-compose.override.yml`, который пробрасывает порт `${APP_PORT:-3000}:3000` на хост, а база данных сохраняется в именованном томе `sqlite_data`.

---

## 5. Авторизация портала Битрикс24

Подключить портал Битрикс24 к серверу можно любым из трёх способов:

### Способ 1: Автоматически через MCP OAuth 2.1 (рекомендуется)
Если задан `BITRIX_PORTAL_DOMAIN` (или приложение уже было установлено в портале), при подключении современного MCP-клиента (Claude Code, Claude Desktop, Codex) откроется окно браузера:
1. Вы подтверждаете доступ на странице согласия MCP-сервера (`/authorize`).
2. Сервер перенаправляет вас на страницу входа Битрикс24 (`https://<portal>/oauth/authorize/`).
3. После входа Битрикс24 возвращает колбэк на `/oauth/bitrix/callback`, сервер шифрует и сохраняет токены в SQLite и выдаёт MCP-клиенту токен, привязанный к созданному подключению.

### Способ 2: Через установку локального приложения в Битрикс24
Нажмите кнопку **Установить / Переустановить** в настройках локального приложения внутри вашего портала Битрикс24. Битрикс24 отправит `POST`-запрос с токенами на `/oauth/bitrix/install`. Сервер проверит токен через `app.info` и `profile`, сохранит подключение в базу и вызовет `BX24.installFinish()`.

### Способ 3: Вручную через `/oauth/bitrix/start` (со статическим токеном)
Если вы используете статический `MCP_AUTH_TOKEN`, вы можете инициировать OAuth-поток напрямую:

```bash
curl -i -H "Authorization: Bearer $MCP_AUTH_TOKEN" \
  "https://your-server.example.com/oauth/bitrix/start?domain=your-portal.bitrix24.ru&mode=write_with_confirmation"
```

Перейдите по URL из заголовка `Location` в браузере и завершите вход в Битрикс24. В ответе вернётся JSON с `connection.id`.

---

## 6. Подключение MCP-клиентов

### Claude Code (OAuth 2.1 или Bearer)

Через интерактивный OAuth 2.1:

```bash
claude mcp add --transport http bitrix24 https://your-server.example.com/mcp
```

Или со статическим токеном `MCP_AUTH_TOKEN`:

```bash
claude mcp add --transport http bitrix24 https://your-server.example.com/mcp \
  --header "Authorization: Bearer <MCP_AUTH_TOKEN>"
```

### OpenAI Codex CLI

```bash
codex mcp add bitrix24 --url https://your-server.example.com/mcp
codex mcp login bitrix24
```

### Cursor / Windsurf / Claude Desktop (JSON-конфигурация со статическим токеном)

```json
{
  "mcpServers": {
    "bitrix24": {
      "url": "https://your-server.example.com/mcp",
      "headers": {
        "Authorization": "Bearer <MCP_AUTH_TOKEN>"
      }
    }
  }
}
```

### Режим Stdio (локальный процесс без HTTP)

Если клиент запускает сервер как дочерний процесс по стандартным потокам ввода-вывода (`stdin`/`stdout`), установите `MCP_TRANSPORT=stdio`:

```json
{
  "mcpServers": {
    "bitrix24": {
      "command": "/path/to/b24",
      "env": {
        "MCP_TRANSPORT": "stdio",
        "APP_URL": "http://localhost:3000",
        "DATABASE_PATH": "/path/to/data/bitrix24-mcp.sqlite",
        "BITRIX_CLIENT_ID": "local.xxxx",
        "BITRIX_CLIENT_SECRET": "xxxx",
        "TOKEN_ENCRYPTION_KEY": "<32-byte-base64-key>",
        "MCP_AUTH_TOKEN": "<32-char-token>"
      }
    }
  }
}
```

> В режиме `stdio` логи сервера всегда пишутся в `stderr`, чтобы не нарушать JSON-RPC протокол в `stdout`. Предварительно авторизуйте портал в той же базе SQLite через HTTP-режим.

---

## Что дальше?

- Изучите [Справочник MCP-инструментов](tools.md), чтобы узнать, какие запросы и операции доступны AI-агенту.
- Ознакомьтесь с [Конфигурацией](configuration.md) для тонкой настройки кэша, таймаутов и прокси.
- Прочитайте руководство по [Деплою в Docker и Dokploy](deployment.md).
