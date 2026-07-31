# Changelog

Формат основан на [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/).

## [Unreleased]

### Исправлено

- **`request.post` на chromedp-бэкенде отправлял двойное URL-кодирование.** Приватная копия
  `buildPostFormHTML` в `server/browser/chromedp` применяла `url.QueryEscape` поверх
  `html.EscapeString`, из-за чего значение `P@ss` приходило на сервер как `P%2540ss` — молча ломая
  любую форму со спецсимволами. chromedp — это резервный бэкенд, на который проект переходит сам при
  отсутствии `chromedriver`, то есть на типовой инсталляции. Исправление в `common.go` было сделано
  давно, но до копии не дошло.
- **Браузеры и временные каталоги утекали при каждом `SIGTERM`.** `main` завершался раньше, чем
  отрабатывал `Shutdown`, поэтому `service.Close()` (единственный вызов `destroyAll`) обычно не
  выполнялся. Дополнительно `Shutdown` прерывал цепочку очистки на первой ошибке, а запрос в полёте
  на момент остановки гарантировал такую ошибку.
- **`log_level` из `init.yaml` игнорировался.** `SaveConfig` его писал, но `applyConfigFile` не
  читал — уровень логирования, выставленный через `/settings`, терялся при следующем старте.
- **Все операции с сессиями сериализовались на время запуска браузера.** `sessionStore.create`
  держал глобальный write-lock во время `factory.New`, включая скачивание драйвера по сети.
- **Обращение к закрытой сессии.** Между выдачей сессии и захватом её мьютекса `destroy` /
  `destroyAll` успевали закрыть браузер; запрос уходил в «труп» и возвращал транспортную ошибку
  вместо `The session doesn't exist.`
- **Экспортёр Prometheus мог отключиться навсегда.** При ошибке остановки старого сервера
  `m.server` оставался указывать на так и не запущенный экземпляр, и все последующие `ApplyConfig`
  пропускали создание нового.
- **Гонка при сохранении настроек.** Два параллельных POST могли потерять обновление, а запись шла
  в фиксированный `init.yaml.tmp` — переплетение двух записей могло опубликовать полуфайл.
- **Двойной `Wait()` на процессе Xvfb.** Горутина внутри `StartXvfb` и `Process.Wait()` из
  `stopDisplay` состязались за один `wait4`; процесс мог остаться зомби.
- **`sessions.list` не отдавал ключ `sessions` при пустом списке**, хотя Python всегда возвращает
  `[]`.
- **`disableMedia: true` «залипал» на всю сессию** — список блокируемых URL выставлялся, но никогда
  не сбрасывался.
- Утечка файлового дескриптора лога geckodriver при `LOG_LEVEL=debug`.
- `Close()` в chromedriver-бэкенде мог блокировать остановку на 30 секунд на каждую зависшую
  сессию; теперь дедлайн 5 секунд и мягкая эскалация `Wait → SIGTERM → SIGKILL`, которая не оставляет
  осиротевший Chrome.
- Ошибка `Emulation.setUserAgentOverride` больше не проглатывается молча — от этого вызова зависит
  решение managed-челленджей.
- `geckodriver` при заданных логине/пароле прокси теперь пишет предупреждение вместо тихой потери
  учётных данных.
- Повторный идентичный GET в `fetchChromeForTestingText`, который не мог сработать.
- `geckodriver` на Windows распаковывался внутрь кэша `chromedriver`.
- Ошибка запуска Chrome при определении версии больше не теряется.

### Безопасность

- **CSRF на `/api/settings`.** Обработчик принимал любой `Content-Type` и не проверял `Origin`,
  поэтому кросс-доменный POST (*simple request*, без preflight) мог подменить `browser_path` /
  `driver_path` и добиться выполнения произвольного бинарника — с сохранением в `init.yaml`.
  Теперь требуется `Content-Type: application/json`, а кросс-сайтовые `Origin` / `Sec-Fetch-Site`
  отклоняются. Аутентификация по-прежнему не добавляется — это осознанное решение (см.
  [SECURITY.md](SECURITY.md)).
- `chrome_for_testing_url` принимается только по `https`; размер скачиваемого архива драйвера
  ограничен.
- `/v1` принимает только `http`/`https`: `file:///etc/passwd` и `chrome://` больше не открываются
  реальным браузером с возвратом HTML.
- HTTP-сервер получил `ReadHeaderTimeout` / `ReadTimeout` / `IdleTimeout` / `MaxHeaderBytes`;
  тела запросов ограничены по размеру. `WriteTimeout` намеренно не задан — solve легально идёт до
  `maxTimeout`.
- Валидация диапазона портов и запрет совпадения основного порта с прометеевским.

### Изменено

- `NewServer` возвращает `(*Server, error)` вместо паники.
- Версия проставляется через `-ldflags -X` из `git describe --tags`; ручная правка
  `internal/buildinfo/version.go` для релиза больше не нужна.
- `build_all.sh`: `mkdir` перед `rm`, `mktemp` вместо фиксированного `/tmp/build_err`, генерация
  `SHA256SUMS`, ненулевой код возврата при ошибке сборки.
- Исправлен нерабочий паттерн `./flaresolverr` в `.gitignore` (git не принимает префикс `./`).

### Добавлено

- Первые тесты: 100+ проверок для конфигурации, настроек, метрик, HTTP-слоя, хелперов
  challenge-пайплайна и `sessionStore` (с подставным browser factory, под `-race`).
- CI на GitHub Actions: `gofmt`, `go mod tidy -diff`, `go vet`, `go build`, `go test -race` плюс
  кросс-компиляция всех 11 целевых платформ.
- `SECURITY.md`, `NOTICE` (атрибуция upstream FlareSolverr под MIT), `CHANGELOG.md`.
- README: описан бэкенд `geckodriver` / Firefox / Camoufox, `browser_backend`,
  `STARTUP_USER_AGENT`, `CHROME_ARGS`, `FIREFOX_ARGS`, `FLARESOLVERR_TMPDIR`,
  `session_ttl_minutes`, `tabs_till_verify`, раздел про тесты.
- `browser_backend` добавлен в поставляемый `init.yaml`.

### Внутреннее

- Из `server/browser/chromedp` удалены приватные копии 19 хелперов, селекторов и структур — теперь
  все три бэкенда используют общие реализации из `server/browser/common.go`. Именно эта копипаста и
  породила баг с двойным кодированием.
- Разделяемый page-side JavaScript вынесен в `server/browser/scripts.go`; два побайтово одинаковых
  DOM-обходчика больше не живут в разных файлах.
- Собственный base64 заменён на `encoding/base64`.
- Суммарно −551 строка в `server/browser/` без потери функциональности.

## [1.0.6]

- Поддержка `FakeShadowRoot` для Turnstile в chromedp и webdriver.

## [1.0.5]

- Бэкенд `geckodriver`, извлечение Turnstile-токена, обновление settings UI.

## [1.0.4]

- Web UI и API управления настройками (`/settings`, `/api/settings`).

## [1.0.3]

- Отслеживание document response, transient-каталоги, кэширование user agent.
