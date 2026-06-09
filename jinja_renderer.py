#!/usr/bin/env python3
"""Line-delimited JSON renderer for Python Jinja2.

Go owns prompt fetching and process lifecycle. This helper only renders a
template string with caller-provided variables using the same Jinja defaults as
Disha's documents.document_manager.DocumentManager.
"""

from __future__ import annotations

import hashlib
import json
import sys
import traceback
from typing import Any

import jinja2
import jinja2.meta


ENV = jinja2.Environment(undefined=jinja2.Undefined)
STRICT_ENV = jinja2.Environment(undefined=jinja2.StrictUndefined)
TEMPLATE_CACHE: dict[str, jinja2.Template] = {}


def cached_template(text: str) -> jinja2.Template:
    key = hashlib.sha256(text.encode("utf-8")).hexdigest()
    template = TEMPLATE_CACHE.get(key)
    if template is None:
        template = ENV.from_string(text)
        TEMPLATE_CACHE[key] = template
    return template


def render(req: dict[str, Any]) -> dict[str, Any]:
    request_id = str(req.get("id") or "")
    text = req.get("template") or ""
    variables = req.get("variables") or {}
    if not isinstance(text, str):
        return error_response(request_id, "request", "template must be a string")
    if not isinstance(variables, dict):
        return error_response(request_id, "request", "variables must be an object")

    compile_time_missing: list[str] = []
    undefined_error: str | None = None
    strict_error: str | None = None

    try:
        parsed = ENV.parse(text)
        required = jinja2.meta.find_undeclared_variables(parsed)
        compile_time_missing = sorted(required - set(variables.keys()))
    except Exception as exc:
        # Match DocumentManager's spirit: parsing errors are warnings until an
        # actual render is requested.
        strict_error = str(exc)

    try:
        STRICT_ENV.from_string(text).render(**variables)
    except jinja2.UndefinedError as exc:
        undefined_error = str(exc)
    except Exception as exc:
        strict_error = str(exc)

    try:
        # DocumentManager returns the raw prompt when variables are None/empty.
        # Preserve that behavior instead of rendering all undefined references
        # away.
        output = cached_template(text).render(**variables) if variables else text
    except Exception as exc:
        return error_response(request_id, "render", str(exc))

    return {
        "id": request_id,
        "ok": True,
        "output": output,
        "compile_time_missing_variables": compile_time_missing,
        "undefined_error": undefined_error,
        "strict_error": strict_error,
    }


def error_response(request_id: str, stage: str, message: str) -> dict[str, Any]:
    return {
        "id": request_id,
        "ok": False,
        "stage": stage,
        "error": message,
        "traceback": traceback.format_exc(limit=6),
    }


def main() -> int:
    for line in sys.stdin:
        if not line.strip():
            continue
        request_id = ""
        try:
            req = json.loads(line)
            request_id = str(req.get("id") or "")
            resp = render(req)
        except Exception as exc:
            resp = error_response(request_id, "request", str(exc))
        print(json.dumps(resp, ensure_ascii=False), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
