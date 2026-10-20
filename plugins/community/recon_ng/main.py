#!/usr/bin/env python3
"""recon_ng — модульный OSINT-фреймворк Recon-ng, неинтерактивный прогон .rcn.

Вход (stdin JSON): target (обязательный), modules (опц., массив путей
модулей), wall_timeout (опц., 600).

Что происходит: плагин сам собирает в temp-папке сценарий run.rcn —
по одному блоку `modules load <m> / options set SOURCE <target> / run / back`
на каждый модуль, затем блок отчёта `modules load reporting/json /
options set filename results.json / run / back`, и в конце `exit` — без него
recon-ng после чтения сценария проваливается в бесконечный цикл prompt'а на
закрытом stdin. Запуск: <RECON_NG_BIN | recon-ng> -w fidespipe_<hash> -r run.rcn
--no-version --no-analytics --no-marketplace (последние три флага гасят
проверку версии, телеметрию и обращение к маркетплейсу; модули берутся из
уже установленных в ~/.recon-ng/modules). stdin закрыт, cwd = temp-папка.

Отчёт: модуль reporting/json пишет results.json — путь передан относительным
именем, поэтому файл попадает в temp-папку, а не в воркспейс recon-ng.

Воркспейс создаётся в ~/.recon-ng/workspaces/fidespipe_<hash> и удаляется после
прогона (имя уникально на цель, состояние не теряется и не копится).

Выход (stdout JSON): {target, modules[], findings[], count}. findings — по
строке на запись отчёта: {table, record{колонка: значение}}. Пустой отчёт =
«ничего не нашлось» (ok, count=0).

Доменные ошибки: empty_target, bad_target, bad_modules, recon_ng_not_installed,
timeout (retryable), no_report, tool_failed. Платформенные (exit 2): битый
JSON входа, нечитаемый results.json.
"""
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

DEFAULT_WALL = 600
REPORT_FILE = "results.json"
REPORT_MODULE = "reporting/json"

# Дефолтный набор — только модули без required_keys из индекса recon-ng-modules
# (modules.yml): crt.sh (certificate transparency) и DNS-резолвер.
DEFAULT_MODULES = ["recon/domains-hosts/certificate_transparency",
                   "recon/hosts-hosts/resolve"]

MODULE_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_./-]*$")
TARGET_RE = re.compile(r"^[^\s\"'<>|;&$`]+$")
WORKSPACE_PREFIX = "fidespipe_"


try:
    sys.stdin.reconfigure(encoding="utf-8")
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass


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
    """Путь к CLI recon-ng: env, потом recon-ng из PATH."""
    bin_env = os.environ.get("RECON_NG_BIN", "").strip()
    if bin_env:
        # subprocess поедет с cwd во временной папке: имя ищем which'ем,
        # путь — приводим к абсолютному
        if "/" in bin_env or "\\" in bin_env:
            return os.path.abspath(bin_env)
        return shutil.which(bin_env) or os.path.abspath(bin_env)
    return shutil.which("recon-ng")


def build_script(target, modules):
    """Текст .rcn-сценария: блок на модуль + блок JSON-отчёта + exit."""
    lines = []
    for module in modules:
        lines += [f"modules load {module}",
                  f"options set SOURCE {target}",
                  "run",
                  "back"]
    lines += [f"modules load {REPORT_MODULE}",
              f"options set filename {REPORT_FILE}",
              "run",
              "back",
              "exit"]
    return "\n".join(lines) + "\n"


def findings_from(report):
    """Плоский разбор results.json: по строке на запись каждой таблицы."""
    out = []
    if not isinstance(report, dict):
        return out
    for table, rows in report.items():
        if isinstance(rows, list):
            for row in rows:
                if not isinstance(row, dict):
                    continue
                record = {}
                for key, value in row.items():
                    record[str(key)] = "" if value is None else (
                        value if isinstance(value, (str, int, float, bool))
                        else json.dumps(value, ensure_ascii=False, default=str))
                out.append({"table": str(table), "record": record})
    return out


def main():
    try:
        data = json.load(sys.stdin)
    except Exception as e:
        return fail("bad_input", f"невалидный JSON на входе: {e}", exit_code=2)
    if not isinstance(data, dict):
        return fail("bad_input", "ожидается JSON-объект на входе", exit_code=2)

    target = str(data.get("target") or "").strip()
    if not target:
        return fail("empty_target", "target пуст")
    # цель уходит в .rcn-сценарий как значение опции — пробелы и кавычки там
    # ломают разбор команд recon-ng
    if len(target) > 253 or not TARGET_RE.match(target):
        return fail("bad_target",
                    "target не похож на домен/хост: пробелы и спецсимволы "
                    "запрещены, длина до 253 символов")

    modules = data.get("modules")
    if modules is None:
        modules = list(DEFAULT_MODULES)
    elif not isinstance(modules, list):
        return fail("bad_modules", "modules обязан быть массивом", exit_code=2)
    else:
        cleaned = []
        for item in modules:
            name = str(item).strip()
            if not MODULE_RE.match(name):
                return fail("bad_modules",
                            f"недопустимое имя модуля: {item!r}")
            cleaned.append(name)
        modules = cleaned

    try:
        wall = float(data.get("wall_timeout") or DEFAULT_WALL)
    except (TypeError, ValueError):
        return fail("bad_wall_timeout", "wall_timeout обязан быть числом", exit_code=2)

    base = resolve_bin()
    if base is None:
        return fail("recon_ng_not_installed",
                    "recon-ng не найден в PATH: pip install recon-ng "
                    "(или укажите RECON_NG_BIN)")

    # v0.29: .py-мок запускаем через интерпретатор (см. holehe)
    cmd = [sys.executable, base] if base.lower().endswith(".py") else [base]

    digest = hashlib.sha1(target.encode("utf-8")).hexdigest()[:10]
    workspace = f"{WORKSPACE_PREFIX}{digest}"
    ws_root = os.path.join(os.path.expanduser("~"), ".recon-ng", "workspaces")
    ws_path = os.path.join(ws_root, workspace)

    with tempfile.TemporaryDirectory() as td:
        script = os.path.join(td, "run.rcn")
        with open(script, "w", encoding="utf-8", newline="\n") as f:
            f.write(build_script(target, modules))
        report_path = os.path.join(td, REPORT_FILE)

        cmd += ["-w", workspace, "-r", script,
                "--no-version", "--no-analytics", "--no-marketplace"]
        try:
            proc = subprocess.run(cmd, capture_output=True, text=True, cwd=td,
                                  stdin=subprocess.DEVNULL, timeout=wall)
        except FileNotFoundError:
            return fail("recon_ng_not_installed",
                        "recon-ng не найден: pip install recon-ng "
                        "(или укажите RECON_NG_BIN)")
        except subprocess.TimeoutExpired:
            return fail("timeout",
                        f"recon-ng не уложился в {wall:.0f}s: сузьте modules "
                        "или увеличьте wall_timeout", retryable=True)
        finally:
            # воркспейс — во временном каталоге ОС не помещается: имя
            # уникально на цель, удаляем только свой (префикс fidespipe_)
            if workspace.startswith(WORKSPACE_PREFIX) and ws_path.startswith(ws_root):
                shutil.rmtree(ws_path, ignore_errors=True)
        if proc.stderr:
            sys.stderr.write(proc.stderr)
        if proc.stdout:
            sys.stderr.write(proc.stdout)

        if proc.returncode != 0:
            tail = (proc.stderr or proc.stdout or "").strip().splitlines()
            last = tail[-1] if tail else f"exit {proc.returncode}"
            return fail("tool_failed",
                        f"recon-ng вернул код {proc.returncode}: {last}")

        if not os.path.isfile(report_path):
            return fail("no_report", "recon-ng не записал results.json")
        try:
            with open(report_path, encoding="utf-8") as f:
                raw = f.read().strip()
        except Exception as e:
            return fail("bad_report", f"не прочитан results.json: {e}",
                        exit_code=2)
        if not raw:
            return fail("no_report", "results.json пуст")
        try:
            report = json.loads(raw)
        except Exception as e:
            return fail("bad_report", f"не прочитан JSON recon-ng: {e}",
                        exit_code=2)

    if not isinstance(report, dict):
        return fail("bad_report",
                    "неожиданный формат results.json recon-ng", exit_code=2)

    findings = findings_from(report)
    return ok({"target": target, "modules": list(modules),
               "findings": findings, "count": len(findings)})


if __name__ == "__main__":
    sys.exit(main())