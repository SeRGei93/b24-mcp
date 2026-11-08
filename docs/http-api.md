# HTTP API и эндпоинты сервера

В режиме `MCP_TRANSPORT=http` (по умолчанию) **Битрикс 24 MCP** запускает HTTP-сервер, маршрутизация которого реализована в [`internal/server/server.go`](../internal/server/server.go). Максимальный размер тела любого входящего запроса ограничен **1 МБ** (`1048576` байт).

---

## Сводная таблица маршрутов

| Метод и путь | Аутентификация | Rate Limit (по IP / глобально) | Назначение |
| --- | --- | --- | --- |
| `GET /` | Публичный | — | Информация о сервере, версии и основных точках входа. |
| `GET /health` | Публичный | — | Проверка доступности сервера и соединения с SQLite (`SELECT 1`). |
| `GET /.well-known/oauth-protected-resource` | Публичный | — | Метаданные защищённого ресурса (RFC 9728). |
| `GET /.well-known/oauth-protected-resource/mcp` | Публичный | — | Метаданные защищённого ресурса для `/mcp` (RFC 9728). |
| `GET /.well-known/oauth-authorization-server` | Публичный | — | Метаданные сервера авторизации OAuth 2.1 (RFC 8414). |
| `POST /register` | Публичный | 20 / 500 за 60 мин | Динамическая регистрация OAuth 2.1 клиента (RFC 7591). |
| `GET /authorize` | Публичный / Bearer | 100 / 2500 за 15 мин | Инициация OAuth 2.1 + PKCE потока (экран согласия или редирект). |
| `POST /authorize` | CSRF Cookie | 100 / 2500 за 15 мин | Обработка решения пользователя (`approve` / `deny`) с экрана согласия. |
| `POST /token` | Публичный (PKCE) | 50 / 1250 за 15 мин | Обмен `authorization_code` или `refresh_token` на токены MCP. |
| `POST /revoke` | Публичный | 50 / 1250 за 15 мин | Отзыв `access_token` или `refresh_token` (RFC 7009). |
| `OPTIONS /register`, `/token`, `/revoke` | Публичный | — | CORS preflight-ответ (`204 No Content`). |
| `GET /oauth/bitrix/start` | `MCP_AUTH_TOKEN` | — | Ручной запуск OAuth 2.0 авторизации портала Битрикс24. |
| `GET /oauth/bitrix/callback` | Одноразовый `state` | — | Обработчик OAuth 2.0 редиректа от Битрикс24. |
| `GET /oauth/bitrix/install` | Публичный | — | Проверка готовности эндпоинта установки приложения Битрикс24. |
| `POST /oauth/bitrix/install` | Токен Битрикс24 | — | Первоначальная установка локального приложения из Битрикс24. |
| `POST /oauth/bitrix/callback` | Токен Битрикс24 | — | Алиас для `POST /oauth/bitrix/install` (на случай отправки POST в Handler path). |
| `POST /oauth/bitrix/refresh` | Bearer (MCP) | — | Принудительное обновление токена Битрикс24 для подключения. |
| `POST /mcp` | Bearer (MCP) | — | Основной эндпоинт протокола MCP (Stateless Streamable HTTP). |
| `GET /mcp`, `DELETE /mcp` | Bearer (MCP) | — | Возвращает `405 Method Not Allowed` в формате JSON-RPC 2.0. |

---

## 1. Системные эндпоинты

### `GET /`
Возвращает краткую карточку сервера:

```json
{
  "name": "bitrix24-mcp-server",
  "version": "0.1",
  "mcp_endpoint": "/mcp",
  "oauth_start": "/oauth/bitrix/start?domain=your-portal.bitrix24.com",
  "bitrix_oauth_callback": "/oauth/bitrix/callback",
  "oauth_metadata": "/.well-known/oauth-authorization-server"
}
```

### `GET /health`
Выполняет `SELECT 1` в базе данных SQLite. Используется Docker `HEALTHCHECK` и системами оркестрации (Dokploy, Kubernetes).
- **200 OK**: `{"status": "ok", "database": "connected"}`
- **503 Service Unavailable**: `{"status": "unavailable", "database": "disconnected"}`

---

## 2. Метаданные и эндпоинты MCP OAuth 2.1

### `GET /.well-known/oauth-protected-resource` и `/mcp`
Кэшируется заголовком `Cache-Control: public, max-age=300`.

```json
{
  "resource": "https://your-server.example.com/mcp",
  "authorization_servers": ["https://your-server.example.com"],
  "scopes_supported": ["mcp:tools"],
  "bearer_methods_supported": ["header"],
  "resource_name": "Bitrix24 MCP Server",
  "resource_documentation": "https://your-server.example.com/"
}
```

### `GET /.well-known/oauth-authorization-server`
```json
{
  "issuer": "https://your-server.example.com",
  "authorization_endpoint": "https://your-server.example.com/authorize",
  "token_endpoint": "https://your-server.example.com/token",
  "registration_endpoint": "https://your-server.example.com/register",
  "revocation_endpoint": "https://your-server.example.com/revoke",
  "response_types_supported": ["code"],
  "grant_types_supported": ["authorization_code", "refresh_token"],
  "token_endpoint_auth_methods_supported": ["none"],
  "revocation_endpoint_auth_methods_supported": ["none"],
  "code_challenge_methods_supported": ["S256"],
  "scopes_supported": ["mcp:tools"],
  "service_documentation": "https://your-server.example.com/"
}
```

### `POST /register` (Dynamic Client Registration)
Принимает JSON с метаданными клиента:
- `redirect_uris` (обязателен, от 1 до 10 URL; разрешены `https://` или локальные `http://localhost` / `127.0.0.1` / `[::1]`).
- `token_endpoint_auth_method`: `"none"` (по умолчанию).
- `grant_types`: `["authorization_code", "refresh_token"]`.
- `response_types`: `["code"]`.
- `client_name`, `client_uri`, `logo_uri`, `scope` (`"mcp:tools"`), `software_id`, `software_version`.

Возвращает `201 Created` со сгенерированным `client_id` вида `mcp_<random>`.

### `GET /authorize` и `POST /authorize`
- `GET /authorize` принимает query-параметры `response_type=code`, `client_id`, `redirect_uri`, `code_challenge`, `code_challenge_method=S256`, `state`, `scope=mcp:tools`, `resource`.
- Отображает HTML-форму подтверждения с заголовками `X-Frame-Options: DENY` и строгой `Content-Security-Policy` и устанавливает cookie `mcp_oauth_csrf` (`HttpOnly; SameSite=Lax; Path=/authorize`).
- `POST /authorize` принимает `request_token`, `csrf_token` и `decision` (`approve` или `deny`) в формате `application/x-www-form-urlencoded` или `application/json`. При `approve` перенаправляет пользователя на OAuth-сервер портала Битрикс24.

### `POST /token`
Поддерживает `application/x-www-form-urlencoded` и `application/json`.
- При `grant_type=authorization_code`: принимает `client_id`, `code`, `code_verifier`, `redirect_uri`, `resource`.
- При `grant_type=refresh_token`: принимает `client_id`, `refresh_token`, `scope`, `resource`.
- Возвращает `200 OK`:
  ```json
  {
    "access_token": "...",
    "token_type": "Bearer",
    "expires_in": 3600,
    "refresh_token": "...",
    "scope": "mcp:tools"
  }
  ```

### `POST /revoke`
Принимает `token` (можно передать как `access_token`, так и `refresh_token`). Помечает запись токена в БД полем `revoked_at` и возвращает `200 OK`.

---

## 3. Эндпоинты интеграции с Битрикс24

### `GET /oauth/bitrix/start`
Инициирует автономный OAuth 2.0 поток с порталом Битрикс24.
- **Заголовок**: `Authorization: Bearer <MCP_AUTH_TOKEN>` (токены OAuth 2.1, привязанные к одному подключению, к этому маршруту не допускаются).
- **Query-параметры**:
  - `domain` (обязателен) — домен портала, например `company.bitrix24.ru`.
  - `mode` (опционален) — `read_only` или `write_with_confirmation` (по умолчанию берётся из `BITRIX_MCP_DEFAULT_MODE`).
- **Ответ**: `302 Found` с редиректом на `https://<domain>/oauth/authorize/?client_id=...&state=...`.

### `GET /oauth/bitrix/callback`
Принимает параметры `code`, `state`, `domain`, `member_id`, `error` от Битрикс24:
- Если авторизация была запущена в рамках **MCP OAuth 2.1**, завершает связку и возвращает `302 Found` на `redirect_uri` MCP-клиента с параметром `code`.
- Если авторизация была запущена через `/oauth/bitrix/start`, возвращает `200 OK`:
  ```json
  {
    "connected": true,
    "connection": {
      "id": "550e8400-e29b-41d4-a716-446655440000",
      "domain": "company.bitrix24.ru",
      "mode": "read_only",
      "status": "active",
      "...": "..."
    },
    "next": "Use this connection.id with the Bitrix24 MCP tools."
  }
  ```

### `POST /oauth/bitrix/install` (и `POST /oauth/bitrix/callback`)
Принимает установочный вебхук от Битрикс24 (поддерживает плоские поля `AUTH_ID`/`REFRESH_ID`, вложенный объект `auth[access_token]` в `application/x-www-form-urlencoded` и JSON).
- При заголовке `Accept: text/html` возвращает HTML-страницу, вызывающую `BX24.installFinish()`.
- Иначе возвращает JSON `{"status": "success", "connected": true, "connection": {...}}`.

### `POST /oauth/bitrix/refresh`
Принудительно обновляет токен Битрикс24 для указанного подключения.
- **Заголовок**: `Authorization: Bearer <token>`
- **Тело запроса**:
  ```json
  {
    "connection_id": "550e8400-e29b-41d4-a716-446655440000"
  }
  ```
- **Ответ**: `200 OK` с полями `{"refreshed": true, "connection": {...}}`.

---

## 4. Эндпоинт протокола MCP (`POST /mcp`)

Реализует **Stateless Streamable HTTP** транспорт спецификации MCP (`github.com/mark3labs/mcp-go/server`, режим `WithStateLess(true)`, `WithDisableStreaming(true)`).
- Требует заголовок `Authorization: Bearer <token>`.
- Если в запросе присутствует заголовок `Origin`, он обязан точно совпадать с `APP_URL` (иначе возвращается `403 ORIGIN_NOT_ALLOWED`).
- При отсутствии или невалидности токена возвращает `401 Unauthorized` с заголовком:
  ```http
  WWW-Authenticate: Bearer resource_metadata="https://your-server.example.com/.well-known/oauth-protected-resource/mcp", scope="mcp:tools", error="invalid_token"
  ```

---

## 5. Коды ошибок HTTP API

Все не-OAuth ошибки возвращаются в формате `{"error": "<CODE>", "message": "<Safe message>"}`:

| HTTP-статус | Код ошибки (`error`) | Когда возникает |
| :---: | --- | --- |
| `400` | `INVALID_REQUEST` | Ошибка валидации тела запроса, query-параметров или UUID. |
| `400` | `INVALID_PORTAL_DOMAIN` | Домен портала не является HTTPS облачным доменом `*.bitrix24.*` и не входит в `BITRIX_PORTAL_DOMAIN`. |
| `401` | `UNAUTHORIZED` | Отсутствует или недействителен Bearer-токен, либо не пройдена проверка OAuth `state` / `profile.json`. |
| `403` | `ORIGIN_NOT_ALLOWED` | Заголовок `Origin` не совпадает с `APP_URL`. |
| `403` | `CONNECTION_ACCESS_DENIED` | Токен MCP привязан к другому `connectionId`. |
| `403` | `POLICY_DENIED` | Подключение неактивно или находится в режиме `read_only` при попытке записи. |
| `404` | `NOT_FOUND` | Запрошенное подключение или операция не найдены. |
| `409` | `OPERATION_NOT_CONFIRMABLE` / `OPERATION_NOT_CANCELLABLE` | Подготовленная операция уже выполнена, отменена или истекла по TTL. |
| `502` | `BITRIX_OAUTH_ERROR` / `NETWORK_ERROR` / `RETRY_EXHAUSTED` | Ошибка на стороне OAuth или REST API Битрикс24. |
| `503` | `BITRIX_PORTAL_NOT_CONFIGURED` | Не задан `BITRIX_PORTAL_DOMAIN` и в базе нет единственного активного портала для авто-редиректа. |
