#!/usr/bin/env python3
"""scoutsuite — аудит облака ScoutSuite (обёртка над CLI `scout`).

Вход (stdin JSON): provider (опц., по умолчанию aws), account (опц., имя
AWS-профиля), wall_timeout (опц., общий лимит, 1800).

Вызов: <SCOUTSUITE_BIN|scout|python3 -m ScoutSuite> <provider>
          --report-dir <tmp>/scout-report --report-name fidespipe
          --result-format json --no-browser [--profile <account>]
       (cwd = временная папка).

Флаги подтверждены по ScoutSuite/core/cli_parser.py: провайдер — позиционный
подпарсер (aws|azure|gcp|aliyun|oci|kubernetes|do), общие --report-dir,
--report-name, --result-format {json,sqlite}, --no-browser, -p/--profile
(только у aws и oci — у azure -p это --password, поэтому --profile передаётся
исключительно для provider=aws). Других флагов не выдумываем.

Отчёт. ScoutSuite пишет не JSON-файл, а JS-обёртку
<--report-dir>/scoutsuite-results/scoutsuite_results_<report_name>.js: первая
строка — `scoutsuite_results =`, дальше JSON (так делает
ScoutSuite/output/utils.py get_filename, документировано в вики «Exporting and
Programmatically Accessing the Report»). Разбираем так же: отбрасываем строку
объявления, режем по первой `{`, снимаем хвостовую `;`.

Находки лежат в services[<service>].findings[<id>] = {level: danger|warning|
info, description, items: [...]}. Каждую группу отдаём как finding
{service, id, level, description, count=len(items)}; count сверху = число групп.
Скан без находок — ok с пустым массивом.

Учётные данные. main.py ключей НЕ читает и не требует: scout сам берёт их из
окружения (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY/AWS_SESSION_TOKEN,
AWS_DEFAULT_REGION). В тестах инструмент подменён моком.

Доменные ошибки: bad_provider, scoutsuite_not_installed, timeout (retryable),
no_report, tool_failed. Платформенные (exit 2): битый JSON входа, нечисловой
wall_timeout, нечитаемый отчёт.
"""
import glob
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

DEFAULT_PROVIDER = "aws"
DEFAULT_WALL = 1800
REPORT_DIR = "scout-report"
REPORT_NAME = "fidespipe"
RESULT_GLOB = os.path.join(REPORT_DIR, "scoutsuite-results",
                           "scoutsuite_results_*.js")

PROVIDERS = ("aws", "azure", "gcp", "aliyun", "oci", "kubernetes", "do")

NOT_MODULE_RE = re.compile(r"No module named", re.IGNORECASE)


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False, exit_code=1):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return exit_code


def resolve_bin():
    bin_env = os.environ.get("SCOUTSUITE_BIN", "").strip()
    if bin_env:
        if "/" in bin_env or "\\" in bin_env:
            return [os.path.abspath(bin_env)]
        return [shutil.which(bin_env) or os.path.abspath(bin_env)]
    found = shutil.which("scout")
    if found:
        return [found]
    return [sys.executable, "-m", "ScoutSuite"]


def strip_js_wrapper(text):
    """Первая строка — `scoutsuite_results =`, дальше JSON. Режем по первой {."""
    body = text.strip()
    if not body:
        raise ValueError("пустой файл отчёта")
    if body[0] not in "{[":
        start = body.find("{")
        if start < 0:
            raise ValueError("в отчёте нет JSON-объекта")
        body = body[start:]
    if body.endswith(";"):
        body = body[:-1]
    return json.loads(body.strip())


def collect_findings(report):
    findings = []
    services = report.get("services")
    if not isinstance(services, dict):
        return findings
    for service in sorted(services):
        block = services.get(service)
        if not isinstance(block, dict):
            continue
        raw = block.get("findings")
        if not isinstance(raw, dict):
            continue
        for finding_id in sorted(raw):
            item = raw[finding_id]
            if not isinstance(item, dict):
                continue
            items = item.get("items")
            findings.append({
                "service": str(service),
                "id": str(finding_id),
                "level": str(item.get("level") or ""),
                "description": str(item.get("description")
                                   or item.get("display_title") or ""),
                "count": len(items) if isinstance(items, list) else 0,
            })
    return findings


def main():
    try:
        sys.stdin.reconfigure(encoding="utf-8", errors="replace")
        sys.stdout.reconfigure(encoding="utf-8")
        sys.stderr.reconfigure(encoding="utf-8", errors="replace")
    except Exception:
        pass

    try:
        data = json.load(sys.stdin)
    except Exception as e:
        return fail("bad_input", f"невалидный JSON на входе: {e}", exit_code=2)
    if not isinstance(data, dict):
        return fail("bad_input", "ожидается JSON-объект на входе", exit_code=2)

    provider = str(data.get("provider") or DEFAULT_PROVIDER).strip().lower()
    if not provider:
        provider = DEFAULT_PROVIDER
    if provider not in PROVIDERS:
        return fail("bad_provider",
                    "provider должен быть одним из: " + ", ".join(PROVIDERS))

    account = str(data.get("account") or "").strip()
    if account and provider != "aws":
        return fail("bad_account",
                    "account — это имя AWS-профиля (--profile) и применим "
                    "только к provider=aws")

    try:
        wall = float(data.get("wall_timeout") or DEFAULT_WALL)
    except (TypeError, ValueError):
        return fail("bad_wall_timeout", "wall_timeout обязан быть числом",
                    exit_code=2)

    cmd = resolve_bin()
    if len(cmd) == 1 and cmd[0].lower().endswith(".py"):
        cmd = [sys.executable] + cmd
    cmd += [provider, "--report-dir", REPORT_DIR, "--report-name", REPORT_NAME,
            "--result-format", "json", "--no-browser"]
    if account:
        cmd += ["--profile", account]

    with tempfile.TemporaryDirectory() as td:
        try:
            proc = subprocess.run(cmd, capture_output=True, text=True,
                                  cwd=td, timeout=wall)
        except FileNotFoundError:
            return fail("scoutsuite_not_installed",
                        "scout не найден: pip install scoutsuite "
                        "(или укажите SCOUTSUITE_BIN)")
        except subprocess.TimeoutExpired:
            return fail("timeout",
                        f"scout не уложился в {wall:.0f}s: сузьте набор "
                        "сервисов/регионов или увеличьте wall_timeout",
                        retryable=True)
        if proc.stderr:
            sys.stderr.write(proc.stderr)

        blob = (proc.stdout or "") + "\n" + (proc.stderr or "")
        if proc.returncode != 0:
            if NOT_MODULE_RE.search(blob):
                return fail("scoutsuite_not_installed",
                            "scoutsuite не установлен: pip install scoutsuite "
                            "(или укажите SCOUTSUITE_BIN)")
            tail = blob.strip().splitlines()
            last = tail[-1] if tail else f"exit {proc.returncode}"
            return fail("tool_failed",
                        f"scout упал (exit {proc.returncode}): {last}")

        matches = sorted(glob.glob(os.path.join(td, RESULT_GLOB)))
        if not matches:
            tail = blob.strip().splitlines()
            last = tail[-1] if tail else "пустой вывод"
            return fail("no_report",
                        f"scout не дал scoutsuite_results_<name>.js: {last}")
        with open(matches[-1], encoding="utf-8", errors="replace") as f:
            text = f.read()

    try:
        report = strip_js_wrapper(text)
    except Exception as e:
        return fail("bad_report", f"не прочитан отчёт scout: {e}", exit_code=2)
    if not isinstance(report, dict):
        return fail("bad_report", "неожиданный формат отчёта scout",
                    exit_code=2)

    findings = collect_findings(report)
    return ok({"provider": provider, "findings": findings,
               "count": len(findings)})


if __name__ == "__main__":
    sys.exit(main())