#!/usr/bin/env python3
"""crtsh — сертификаты по домену через crt.sh (Certificate Transparency).

Вход (stdin JSON): domain, timeout (опц., с, default 30).

HTTP: GET https://crt.sh/?q=%25.<domain>&output=json — wildcard-запрос
(%domain) ловит все поддомены. Ответ — массив записей: {common_name,
name_value, issuer_name, not_before, not_after, serial_number, ...}.
name_value в API — имена через \n (иногда через запятую) — режем по обоим.

Выход (stdout JSON): {domain, total, names[], issuers[], expired}.
names — дедупликация CN + SAN; issuers — дедупликация issuer_name;
expired — записи с not_after < now(UTC). Сырьё (тысячи записей) в выход
не идёт — только агрегаты.

Тест-режим: CRT_SH_MOCK_FILE — тело HTTP-ответа читать из файла (как
LLM_MOCK у llm_openai — без сети в CI).

Доменные ошибки: empty_domain, timeout (retryable), bad_response
(HTTP != 200, retryable — crt.sh нестабилен). Платформенные (exit 2):
невалидный JSON входа, нечитаемый mock-файл, ответ не-JSON (формат API
изменился).
"""
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone

API = "https://crt.sh/?q=%s&output=json"
DEFAULT_TIMEOUT = 30
NAME_SPLIT = re.compile(r"[,\n]")


def ok(output):
    print(json.dumps({"status": "ok", "output": output}))
    return 0


def fail(code, message, retryable=False, exit_code=1):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}}))
    return exit_code


def parse_date(s):
    try:
        return datetime.strptime(s, "%Y-%m-%dT%H:%M:%S").replace(
            tzinfo=timezone.utc)
    except (TypeError, ValueError):
        return None


def main():
    try:
        data = json.load(sys.stdin)
    except Exception as e:
        return fail("bad_input", f"невалидный JSON на входе: {e}", exit_code=2)
    if not isinstance(data, dict):
        return fail("bad_input", "ожидается JSON-объект на входе", exit_code=2)

    domain = str(data.get("domain") or "").strip().lower()
    if not domain:
        return fail("empty_domain", "domain пуст")

    try:
        timeout = float(data.get("timeout") or DEFAULT_TIMEOUT)
    except (TypeError, ValueError):
        return fail("bad_timeout", "timeout обязан быть числом", exit_code=2)

    mock_file = os.environ.get("CRT_SH_MOCK_FILE", "").strip()
    if mock_file:
        # тест-режим: тело ответа из файла, сети нет
        try:
            with open(os.path.abspath(mock_file), encoding="utf-8") as f:
                body = f.read()
            http_code = 200
        except Exception as e:
            return fail("bad_response", f"CRT_SH_MOCK_FILE не читается: {e}",
                        exit_code=2)
    else:
        url = API % urllib.parse.quote("%." + domain, safe="")
        try:
            req = urllib.request.Request(
                url, headers={"User-Agent": "fidespipe-crtsh/0.1"})
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                body = resp.read().decode("utf-8", "replace")
                http_code = resp.status
        except urllib.error.HTTPError as e:
            return fail("bad_response",
                        f"crt.sh вернул HTTP {e.code}", retryable=True)
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            return fail("timeout",
                        f"crt.sh недоступен ({e}): повторов позволяет "
                        "on_error: retry", retryable=True)

    if http_code != 200:
        return fail("bad_response", f"crt.sh вернул HTTP {http_code}",
                    retryable=True)

    try:
        records = json.loads(body)
    except Exception as e:
        return fail("bad_json", f"crt.sh ответил не-JSON: {e}", exit_code=2)
    if not isinstance(records, list):
        return fail("bad_json", "неожиданный формат реестра crt.sh",
                    exit_code=2)

    now = datetime.now(timezone.utc)
    names = []
    issuers = []
    expired = 0
    for rec in records:
        for name in NAME_SPLIT.split(str(rec.get("name_value") or "")):
            name = name.strip().lstrip("*.")
            if name and name not in names:
                names.append(name)
        cn = str(rec.get("common_name") or "").strip()
        if cn and cn not in names:
            names.append(cn)
        issuer = str(rec.get("issuer_name") or "").strip()
        if issuer and issuer not in issuers:
            issuers.append(issuer)
        not_after = parse_date(rec.get("not_after"))
        if not_after and not_after < now:
            expired += 1

    return ok({"domain": domain, "total": len(records),
               "names": names, "issuers": issuers, "expired": expired})


if __name__ == "__main__":
    sys.exit(main())
