"""A framework-independent tool for asking Foundry, using Python's standard library.

Set FOUNDRY_API_URL, FOUNDRY_ONTOLOGY_ID and FOUNDRY_API_TOKEN in the application's
environment. Bind ask_foundry(question) as an agent tool. Credentials and ontology
selection stay in application configuration, outside the model's arguments.
"""

import json
import os
import time
import uuid
from http.client import HTTPException
from typing import Optional
from urllib.error import HTTPError
from urllib.parse import quote, urljoin, urlsplit
from urllib.request import Request, urlopen


def ask_foundry(question: str, *, idempotency_key: Optional[str] = None, timeout: float = 120) -> dict:
    """Return the question and cited answer, or its pending state at the deadline.

    A pending response includes poll_url; resume with authenticated GETs. Failed,
    cancelled and cannot_answer are terminal statuses, not reasons to ask again.

    idempotency_key is for the application, not the model: pass the key a TimeoutError
    named to recover a question whose response never arrived, rather than ask again.
    """
    base = os.environ["FOUNDRY_API_URL"].rstrip("/")
    ontology = quote(os.environ["FOUNDRY_ONTOLOGY_ID"], safe="")
    key = idempotency_key or str(uuid.uuid4())
    headers = {
        "Authorization": "Bearer " + os.environ["FOUNDRY_API_TOKEN"],
        "Content-Type": "application/json",
        "Idempotency-Key": key,
    }
    url = f"{base}/v1/ontologies/{ontology}/ask"
    payload = json.dumps({"question": question}).encode()
    deadline = time.monotonic() + timeout
    pending = None
    while time.monotonic() < deadline:
        try:
            # Retain the same POST and key across network failures, including a
            # lost response after the backend committed the durable question.
            with urlopen(Request(url, data=payload, headers=headers),
                         timeout=min(35, max(0.1, deadline - time.monotonic()))) as response:
                result = json.load(response)
                if response.status == 200:
                    return result
                if response.status != 202:
                    raise RuntimeError(f"Unexpected Foundry status {response.status}")
                location = urljoin(base, response.headers["Location"])
                if urlsplit(location)[:2] != urlsplit(base)[:2]:
                    raise RuntimeError("Foundry returned a poll URL on another origin")
                pending = {**result, "poll_url": location}
                url, payload = location, None
                delay = min(5, max(1, int(response.headers.get("Retry-After", "1"))))
        except HTTPError as error:
            if error.code not in (408, 500, 502, 503, 504):
                raise RuntimeError(f"Foundry HTTP {error.code}: {error.read().decode()}") from error
            delay = 1
        except (OSError, HTTPException, ValueError):
            # A lost response can surface at any step: refused or reset connections and
            # timeouts are OSError (URLError among them), a dropped response or a cut-off
            # body HTTPException, and a body that parses as nothing ValueError.
            delay = 1
        time.sleep(min(delay, max(0, deadline - time.monotonic())))
    if pending is not None:
        return pending
    # The caller can recover a POST whose response never arrived with this key.
    raise TimeoutError(f"Foundry deadline exceeded; call ask_foundry again with idempotency_key={key!r}")
