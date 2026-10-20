# Сеть недоверенного плагина на Windows

Как выдать внешнему коду сеть в контейнере AppContainer, как это проверить и
что делать, если capability не действует.

## Коротко

| | |
|---|---|
| Как выдаётся | capability `internetClient` (SID **S-1-15-3-1**) в списке `SECURITY_CAPABILITIES` при создании процесса |
| Кому выдаётся | **процессу**, а не профилю: один профиль на плагин обслуживает и плагины с сетью, и без неё |
| Что даёт | выход в интернет; **loopback хоста недоступен** (документация: «No local network access») |
| Чего не даёт | фильтра по хостам: право выдаётся целиком, как и на Linux (там тоже только blanket-вариант) |
| Если не действует | плагин с сетью **не запускается**, с внятным объяснением (fail-closed) |

## Как проверить на машине

```bash
wedra doctor
```

Строка в отчёте:

```
Изолятор: appcontainer; сеть контейнеру: ДОСТУПНА (capability internetClient действует)
```

или

```
Изолятор: appcontainer; сеть контейнеру: НЕДОСТУПНА (NET_FAIL ...)
```

или

```
Изолятор: appcontainer; сеть контейнеру: НЕ ПРОВЕРЕНА (<ошибка пробы>)
```

**«ДОСТУПНА»** — capability работает, плагин с объявленной сетью получит её.

**«НЕДОСТУПНА»** — capability не действует. Обычная причина: в системе нет
таблицы capability (`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\CapabilityTable`),
и тогда SID capability системой не опознаётся. Проверить:

```powershell
reg query "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\CapabilityTable\Capabilities"
Get-Service -Name 'NetworkIsolation*'
```

**«НЕ ПРОВЕРЕНА»** — проба не дошла до ответа (например, нет прав менять DCL).
Тогда смотрите журнал.

## Где живёт проба

| | |
|---|---|
| Код | `internal/sandboxexec/probe.go` — `ProbeNetwork` |
| Самопроверка | `internal/sandboxexec/netself.go` — скрытый режим `internal-sandbox-netself` |
| Вызов | `internal/plugin/sandbox_windows.go` — `networkCapabilityWorks`, один раз на процесс |
| Решение | там же: capability не действует → плагин с сетью отвергается с объяснением |

Проба поднимает контейнер с capability и просит его **создать сокет**. Соединение
не проверяется намеренно: без сети внутри контейнера не инициализируется
Winsock, и падает именно создание сокета, — так что проверка не зависит от
наличия интернета на машине.

Две неудачи различаются по-разному:

- `connection refused` — сокет создан, стек сети работает: **сеть есть**;
- любая другая ошибка (`The requested service provider could not be loaded or
  initialized`) — Winsock в контейнере не поднялся: **сети нет**.

## Журнал

```powershell
Get-WinEvent -LogName 'Microsoft-Windows-AppModel-Runtime/Admin' -MaxEvents 20 |
  ForEach-Object { 'id=' + $_.Id + ' ' + ($_.Message -replace '\s+',' ') }
```

Записи WEDRA в нём выглядят так:

```
id=40  AppContainer ... WEDRA plugin <профиль> не создан, но он уже создан, и это нормально.
id=41  AppContainer <профиль> создан.
id=39  AppContainer <профиль> запущен.
```

Профиль проб�� имеет имя `wedra.netprobe`.

`CheckNetIsolation.exe` показывает состояние только для **пакетных** приложений
(её `-cac` — про «контейнеры, установившие пакет с AppContainer»), поэтому
для непакетного контейнера WEDRA она не показатель.

## Если capability надо зарегистрировать

Магазин capability — **машинный** (`HKLM`), и его запись требует прав
администратора. На обычных клиентах Windows 10/11 он уже содержит
`internetClient`, и ничего регистрировать не нужно. На урезанных образах
(CI-раннеры, Windows Server) его нет, и там запись capability — это
`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\CapabilityTable\Capabilities\internetClient`
со значением `{CapabilityType}` = 2 (сеть) и `{CanonicalName}` = `internetClient`.

Запись в `HKCU` есть только у **пакетных** приложений (репозиторий AppModel),
и к непакетному контейнеру WEDRA она не относится.

## Что делать, если capability не действует

1. **Плагин с сетью на Windows не запустится** — так и должно быть: сеть была
   обещана плагину в манифесте, а выполнить обещание нечем. Молчаливый запуск
   без сети выглядел бы как «плагин сломался», и чинить его стали бы в
   плагине, а не в песочнице.
2. Обход — **доверие по хэшу**: внесите `id@sha256:…` в `trusted_plugins` в
   `wedra-trust.yaml`. Такой плагин идёт с правами пользователя и изоляции не
   получает — это осознанный выбор человека, а не молчаливое ослабление.
3. Перенести цепочку на Linux, где сеть выдаётся через userspace-стек и
   прокси с фильтром.

## Измерено на опорном хосте

Windows 10 Enterprise 22H2 (10.0.19045.7807), не elevated, не-английская
локаль:

- `HKLM\...\CapabilityTable` — **отсутствует**;
- служба `NetworkIsolation*` — **отсутствует**;
- capability в токен добавляется (SID S-1-15-3-1 построен и проверен), но сеть
  не появляется: `The requested service provider could not be loaded or
  initialized` — и с capability, и без него;
- то же самое на CI-раннере (Windows Server, образ урезан).

Отсюда и решение: **fail-closed везде, где capability не действует**, и проба
на хосте, а не предположение.
