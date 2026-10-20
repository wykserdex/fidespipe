# recon_ng

Модульный OSINT-фреймворк Recon-ng (recon-ng/recon-ng): плагин собирает
`.rcn`-сценарий из цели и списка модулей, запускает `recon-ng -r <script>` и
возвращает плоский список находок из JSON-отчёта.

Установка внешнего инструмента: `pip install recon-ng`. Путь к бинарю
переопределяется env `RECON_NG_BIN`.

Что нужно учесть:

- **Модули ставятся отдельно, из маркетплейса.** Свежая установка recon-ng не
  содержит ни одного модуля: они лежат в `~/.recon-ng/modules` и ставятся
  командой `marketplace install all` (или поимённо). Плагин запускает
  `--no-marketplace` — индекс не обновляется, модули берутся уже установленные.
- **Дефолтный набор** (`modules` опционален) — только модули без api-ключей,
  по индексу recon-ng-modules: `recon/domains-hosts/certificate_transparency`
  (crt.sh) и `recon/hosts-hosts/resolve` (DNS-резолвер). Остальные модули
  `recon/...` без ключей тоже работоспособны, например `recon/domains-hosts/hackertarget`,
  `recon/domains-hosts/ssl_san`, `recon/domains-hosts/mx_spf_ip`,
  `recon/ports-hosts/ssl_scan`. Модули с `required_keys` (shodan, github, censys,
  hibp и т.п.) по умолчанию не берутся.
- **Воркспейс.** Прогон идёт в `~/.recon-ng/workspaces/fidespipe_<hash цели>`, он
  создаётся на каждый запуск и удаляется после — состояние не копится. Поэтому
  в манифесте `filesystem: readwrite`.
- **Отчёт.** Модуль `reporting/json` пишет `results.json`; плагин задаёт ему
  относительное имя файла, поэтому отчёт попадает во временную папку прогона.
- Сценарий обязан заканчиваться `exit`: без него recon-ng после чтения файла
  проваливается в бесконечный цикл prompt'а на закрытом stdin.