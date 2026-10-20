#!/usr/bin/env python3
"""threatfox — поиск IOC в базе abuse.ch ThreatFox (паттерн C, сниппет urllib).

Вход (stdin JSON): indicator (ip, ip:port, домен, URL, md5/sha1/sha256),
wall_timeout (опц., общий лимит рана, 60).

Сеть делает только ДОЧЕРНИЙ процесс: тот же интерпретатор с -c SNIPPET (или
THREATFOX_BIN, если он задан — единственный путь для тестов). Сниппет делает
POST https://threatfox-api.abuse.ch/api/v1/ с телом
{"query": "search_ioc", "search_term": <ioc>, "exact_match": true} и печатает в
stdout {"raw": <ответ API>}. Ответ: {"query_status": "ok", "data": [{id, ioc,
threat_type, ioc_type, malware, malware_printable, confidence_level, ...}]}.
Auth-Key abuse.ch требует для Community API, но он необязателен здесь: сниппет
добавляет заголовок только если задан env THREATFOX_AUTH_KEY; ключа в коде нет.
Надёжного pip-пакета нет: `threatfox` на PyPI — один мейнтейнер, релиз 2022 года.

Выход (stdout JSON): {indicator, threat_type, malware, confidence, found}.
Найдена первая запись базы; IOC в базе отсутствует — это ok с found:false и
пустыми полями, а НЕ ошибка. Доменные ошибки: empty_indicator, bad_indicator,
threatfox_not_installed, timeout (retryable), no_report, tool_failed.
Платформенные (exit 2): битый JSON входа, нечитаемый/не-JSON ответ донора.
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile

DEFAULT_WALL = 60
MAX_INDICATOR = 512

SNIPPET = (
    "import json, os, sys, urllib.error, urllib.request\n"
    "API = 'https://threatfox-api.abuse.ch/api/v1/'\n"
    "indicator = sys.argv[1]\n"
    "timeout = float(sys.argv[2]) if len(sys.argv) > 2 else 30.0\n"
    "body = json.dumps({'query': 'search_ioc', 'search_term': indicator,\n"
    "                   'exact_match': True}).encode('utf-8')\n"
    "req = urllib.request.Request(API, data=body, method='POST',\n"
    "    headers={'Content-Type': 'application/json',\n"
    "             'User-Agent': 'fidespipe-threatfox/0.1'})\n"
    "key = os.environ.get('THREATFOX_AUTH_KEY', '').strip()\n"
    "if key:\n"
    "    req.add_header('Auth-Key', key)\n"
    "try:\n"
    "    with urllib.request.urlopen(req, timeout=timeout) as resp:\n"
    "        text = resp.read().decode('utf-8', 'replace')\n"
    "except urllib.error.HTTPError as exc:\n"
    "    print(json.dumps({'http_status': exc.code, 'error': 'http'}))\n"
    "    sys.exit(1)\n"
    "try:\n"
    "    parsed = json.loads(text)\n"
    "except ValueError:\n"
    "    print(json.dumps({'error': 'not_json'}))\n"
    "    sys.exit(1)\n"
    "print(json.dumps({'raw': parsed}))\n"
)


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


def to_number(value):
    if isinstance(value, bool) or value is None:
        return 0
    try:
        num = float(value)
    except (TypeError, ValueError):
        try:
            num = float(str(value).strip())
        except (TypeError, ValueError):
            return 0
    return int(num) if num.is_integer() else num


def build_cmd(indicator, wall):
    bin_env = os.environ.get("THREATFOX_BIN", "").strip()
    if bin_env:
        # имя из PATH ищем which'ем, путь приводим к абсолютному
        if "/" in bin_env or "\\" in bin_env:
            cmd = [os.path.abspath(bin_env)]
        else:
            cmd = [shutil.which(bin_env) or os.path.abspath(bin_env)]
        if len(cmd) == 1 and cmd[0].lower().endswith(".py"):
            # .py-мок запускаем через интерпретатор — прямой exec непереносим
            cmd = [sys.executable] + cmd
    else:
        cmd = [sys.executable, "-c", SNIPPET]
    return cmd + [indicator, str(wall)]


def http_hint(stdout):
    try:
        envelope = json.loads((stdout or "").strip())
    except Exception:
        return None
    if isinstance(envelope, dict) and envelope.get("http_status"):
        return envelope
    return None


def main():
    try:
        data = json.load(sys.stdin)
    except Exception as e:
        return fail("bad_input", f"невалидный JSON на входе: {e}", exit_code=2)
    if not isinstance(data, dict):
        return fail("bad_input", "ожидается JSON-объект на входе", exit_code=2)

    indicator = str(data.get("indicator") or "").strip()
    if not indicator:
        return fail("empty_indicator", "indicator пуст")
    if len(indicator) > MAX_INDICATOR or any(
            ch.isspace() or ord(ch) < 0x21 for ch in indicator):
        return fail("bad_indicator",
                    f"некорректный IOC (пробелы/управляющие символы или длиннее "
                    f"{MAX_INDICATOR}): {indicator[:64]!r}")

    try:
        wall = float(data.get("wall_timeout") or DEFAULT_WALL)
    except (TypeError, ValueError):
        return fail("bad_wall_timeout", "wall_timeout обязан быть числом",
                    exit_code=2)

    cmd = build_cmd(indicator, wall)
    with tempfile.TemporaryDirectory() as td:
        try:
            proc = subprocess.run(cmd, capture_output=True, text=True,
                                  cwd=td, timeout=wall)
        except FileNotFoundError:
            return fail("threatfox_not_installed",
                        "донор threatfox не найден (укажите THREATFOX_BIN)")
        except subprocess.TimeoutExpired:
            return fail("timeout",
                        f"threatfox не уложился в {wall:.0f}s: увеличьте "
                        "wall_timeout", retryable=True)
        if proc.stderr:
            sys.stderr.write(proc.stderr)
        stdout = proc.stdout or ""

        if proc.returncode != 0:
            hint = http_hint(stdout)
            if hint:
                status = hint.get("http_status")
                if status in (401, 403):
                    return fail("tool_failed",
                                f"abuse.ch отклонил запрос без Auth-Key "
                                f"(HTTP {status}): задайте env "
                                "THREATFOX_AUTH_KEY")
                return fail("tool_failed",
                            f"abuse.ch вернул HTTP {status}",
                            retryable=str(status).startswith("5"))
            tail = (proc.stderr or "").strip().splitlines()
            last = tail[-1] if tail else f"exit {proc.returncode}"
            network = ("URLError" in last or "timed out" in last
                       or "Connection" in last)
            return fail("tool_failed", f"запрос к ThreatFox не удался: {last}",
                        retryable=network)

        raw = stdout.strip()

    if not raw:
        return fail("no_report", "дочерний процесс не вернул JSON в stdout")
    try:
        envelope = json.loads(raw)
        if not isinstance(envelope, dict):
            raise ValueError("ожидался объект")
        response = envelope.get("raw", envelope)
    except Exception as e:
        return fail("bad_report", f"не разобран ответ ThreatFox: {e}",
                    exit_code=2)
    if not isinstance(response, dict):
        return fail("bad_report", "неожиданный формат ответа ThreatFox",
                    exit_code=2)

    entries = response.get("data")
    if isinstance(entries, dict):
        entries = [entries] if entries else []
    elif not isinstance(entries, list):
        entries = []
    entry = entries[0] if entries and isinstance(entries[0], dict) else {}

    return ok({
        "indicator": indicator,
        "threat_type": str(entry.get("threat_type") or ""),
        "malware": str(entry.get("malware") or ""),
        "confidence": to_number(entry.get("confidence_level")),
        "found": bool(entry),
    })


if __name__ == "__main__":
    sys.exit(main())