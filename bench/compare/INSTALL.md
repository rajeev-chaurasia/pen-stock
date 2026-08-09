# Installing LiteLLM user scoped, without elevation

This is the transcript of how the LiteLLM side of the comparison was
built, written down because a comparison whose loser cannot be rebuilt
is not checkable. Nothing here is installed system wide, nothing needs
an administrator, and nothing outside `$HOME` is written to.

## The approach

`uv` fetches and manages its own user scoped CPython, so no interpreter
needs to exist on `PATH` and no machine wide package manager is
involved. That keeps the install reproducible on a machine you do not
administer, and keeps it from colliding with whatever Python is already
there.

## The install

```bash
export PATH="$HOME/.local/bin:$PATH"

uv venv --python 3.12 "$HOME/sdk/litellm-venv"

uv pip install --python "$HOME/sdk/litellm-venv/Scripts/python.exe" \
    'litellm[proxy]' httptools 'fastapi<0.140'
```

`uv` already had a managed CPython 3.12.13 on disk, so no interpreter
download was needed. The venv lives at `$HOME/sdk/litellm-venv`.

## The three things that had to be fixed, and why none of them is a thumb on the scale

### 1. `fastapi<0.140` is a required pin, not a preference

`litellm[proxy]` declares `fastapi>=0.136.3,<1.0`, so a plain install
resolves fastapi 0.141.1. LiteLLM 1.95.0 then cannot import at all:

```
File ".../litellm/proxy/management_endpoints/management_v1/common.py", line 6
    from fastapi.dependencies.utils import get_flat_dependant
ImportError: cannot import name 'get_flat_dependant' from
'fastapi.dependencies.utils'
```

fastapi removed that symbol in 0.140. LiteLLM's upper bound is simply
wrong for this release. The CLI hides the real cause behind a fallback
`from proxy_server import ...`, which fails with a misleading
`ModuleNotFoundError: No module named 'proxy_server'`.

Bisecting: 0.140.13 fails, 0.139.2 works. The pin is `fastapi<0.140`,
which resolves to **0.139.2**. This is the newest fastapi LiteLLM 1.95.0
can run on. Nothing here is downgraded further than it has to be.

### 2. `httptools` is installed on purpose, and it helps LiteLLM

Without `httptools`, uvicorn falls back to its pure-Python `h11` HTTP
parser. `httptools` is what `uvicorn[standard]` pulls in and it is the
faster path. Installing it makes LiteLLM **faster**, which is the point:
leaving it out would have been the easy way to quietly win.

### 3. `PYTHONIOENCODING=utf-8` is a Windows fix, not a tuning knob

LiteLLM prints a startup banner containing characters cp1252 cannot
encode. Python on Windows defaults stdout to cp1252, so the proxy died
during ASGI startup:

```
File ".../litellm/proxy/common_utils/banner.py", line 15, in show_banner
    click.echo(f"\n{LITELLM_BANNER}\n")
UnicodeEncodeError: 'charmap' codec can't encode characters in
position 5-7: character maps to <undefined>
```

`PYTHONIOENCODING=utf-8` is the standard fix and does not touch request
handling.

## What could not be installed, and who it costs

**`uvloop` does not exist for Windows.**

```
$ uv pip install uvloop
RuntimeError: uvloop does not support Windows at the moment
```

uvloop replaces asyncio's event loop with a libuv-backed one and is a
material speedup for asyncio servers. On Linux, `uvicorn[standard]`
installs it and LiteLLM would use it. **This is a real handicap that
this platform imposes on LiteLLM and it is not present in Penstock's
column**, because Go's scheduler does not have an equivalent optional
component that Windows withholds.

**`gunicorn` cannot run on Windows.** It needs `fcntl`. LiteLLM's
`--run_gunicorn` flag describes gunicorn as "better for managing
multiple workers", so uvicorn's supervisor is a second-choice
multi-worker path here.

Both are listed in `docs/comparison.md` under the section on what could
still be unfair. Neither can be fixed on this machine, and neither is
hidden.

## Versions actually used

Read the committed `bench/results/compare-*.meta.json` for the exact
values from the published run. At the time of writing:

| Component | Version |
|---|---|
| litellm | 1.95.0 (latest available) |
| Python | CPython 3.12.13 (uv managed, user scoped) |
| uvicorn | 0.52.1 |
| fastapi | 0.139.2 (pinned, see above) |
| httptools | 0.8.0 |
| uvloop | absent, unavailable on Windows |
| orjson | 3.11.9 |
| pydantic | 2.13.4 |

## Running it

```bash
export PATH="$HOME/sdk/go/bin:$HOME/sdk/bin:$PATH"
bash bench/compare/run.sh
```

Or start just the proxy, exactly as the harness does:

```bash
bash bench/compare/start-litellm.sh
curl -s localhost:8081/health/liveliness
```
