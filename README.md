# flaresolverr-go

Go-порт [FlareSolverr](https://github.com/FlareSolverr/FlareSolverr) с совместимыми endpoint'ами `/`, `/health`, `/v1`.

## Что реализовано

- `sessions.create`
- `sessions.list`
- `sessions.destroy`
- `request.get`
- `request.post`
- постоянные browser-сессии
- cookies, screenshot, `waitInSeconds`, `disableMedia`, `tabs_till_verify`
- браузерный слой полностью на Go
- при наличии `chromedriver` по умолчанию используется WebDriver backend с patched `chromedriver`
- если `chromedriver` не найден, проект пытается автоматически скачать matching driver через Chrome for Testing
- если авто-скачивание недоступно, проект откатывается к `chromedp`
- proxy auth через DevTools `Fetch.authRequired`, без Python helper-а
- Prometheus exporter на отдельном порту и Go-плагины `logger`, `error`, `prometheus`

## Архитектура

- HTTP API, session/storage и browser runtime реализованы на Go
- основной код модуля находится в каталоге `server/`
- browser runtime разнесён по `server/browser/chromedp` и `server/browser/webdriver`, общий слой лежит в `server/browser`
- основной runtime: Chrome/Chromium + patched `chromedriver` через WebDriver
- если системного `chromedriver` нет, драйвер подбирается по версии локального Chrome и кэшируется локально
- fallback runtime: `chromedp`, если `chromedriver` недоступен и auto-download не сработал
- на Linux при `HEADLESS=true` используется скрытый headful-режим через `DISPLAY` или `Xvfb`, иначе проект откатывается к обычному Chrome headless

## Зависимости

- Go `1.26+`
- установленный Chrome/Chromium
- установленный `chromedriver` необязателен
- `Xvfb` опционально для Linux `HEADLESS=true`, если нет активного `DISPLAY`

## Запуск

Запуск сервера:

```bash
go run ./cmd/flaresolverr
```

Пример запроса:

```bash
curl -sS -X POST http://127.0.0.1:8191/v1 \
  -H 'Content-Type: application/json' \
  --data '{"cmd":"request.get","url":"https://example.com","maxTimeout":60000}'
```

## Конфиг

Программа читает `init.yaml`:

- сначала из текущей рабочей директории
- если там файла нет, рядом с бинарником

Приоритет настроек:

- встроенные defaults
- `init.yaml`
- переменные окружения
- CLI flags

Если `init.yaml` отсутствует, битый или содержит неверный YAML, программа не падает. Файл будет проигнорирован, а предупреждение уйдёт в лог при старте.

В репозитории лежит пример/дефолтный файл [init.yaml](init.yaml).

## Переменные окружения

- `HOST`, `PORT`
- `BROWSER_PATH`
- `DRIVER_PATH`
- `DRIVER_AUTO_DOWNLOAD`
- `DRIVER_CACHE_DIR`
- `CHROME_FOR_TESTING_URL`
- `HEADLESS`
- `DISABLE_MEDIA`
- `LOG_HTML`
- `PROMETHEUS_ENABLED`
- `PROMETHEUS_PORT`
- `PROXY_URL`, `PROXY_USERNAME`, `PROXY_PASSWORD`

## Сборка

Скрипт [`build_all.sh`](build_all.sh) собирает бинарники для поддерживаемых платформ в каталог `./Dist`:

```bash
./build_all.sh
```

Текущая матрица:

- `linux`: `amd64`, `arm64`, `arm`, `386`
- `darwin`: `amd64`, `arm64`
- `windows`: `amd64`, `arm64`, `386`
- `freebsd`: `amd64`, `arm64`

## systemd

Готовый unit лежит в [`flaresolverr-go.service`](flaresolverr-go.service).

Для серверов без реального дисплея предпочтителен именно внешний `xvfb-run`, а не внутренний автозапуск `Xvfb` внутри процесса. Это ближе к Linux-режиму original FlareSolverr и обычно стабильнее на сложных Cloudflare challenge.

Ожидаемая раскладка на сервере:

- бинарник: `/opt/flaresolverr-go/flaresolverr`
- конфиг: `/opt/flaresolverr-go/init.yaml`
- рабочий каталог: `/opt/flaresolverr-go`
- данные Chrome/XDG: `/var/lib/flaresolverr`
- пользователь: `flaresolverr`
- `Xvfb` должен быть установлен в `/usr/bin/xvfb-run`

Минимальная установка:

```bash
sudo useradd --system --home /var/lib/flaresolverr --shell /usr/sbin/nologin flaresolverr
sudo mkdir -p /opt/flaresolverr-go /var/lib/flaresolverr
sudo chown -R flaresolverr:flaresolverr /var/lib/flaresolverr
sudo cp ./Dist/flaresolverr-linux-amd64 /opt/flaresolverr-go/flaresolverr
sudo cp ./init.yaml /opt/flaresolverr-go/init.yaml
sudo chmod +x /opt/flaresolverr-go/flaresolverr
sudo cp ./flaresolverr-go.service /etc/systemd/system/flaresolverr-go.service
sudo systemctl daemon-reload
sudo systemctl enable --now flaresolverr-go
```

Проверка:

```bash
curl -sS http://127.0.0.1:8191/health
sudo systemctl status flaresolverr-go
```

В этом unit переменные `HOST` / `PORT` / `HEADLESS` / `LOG_LEVEL` не заданы специально, чтобы их задавал `init.yaml`. Если нужно переопределение через `systemd`, добавляй отдельные `Environment=` строки осознанно: они имеют более высокий приоритет, чем файл.
