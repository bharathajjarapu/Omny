# Omny

Omny is a small, self-hosted Go gateway for OpenAI-compatible APIs. Put several
providers and keys behind one endpoint. If a key or provider fails, omny tries
the next one.

It keeps the request body opaque. It reads `model` and `stream`, rewrites the
model name when needed, and forwards the rest unchanged.

## Quick start

```bash
make
cp omny.example.yaml omny.yaml
chmod 600 omny.yaml
```

Set `token:` in `omny.yaml`, then add a key and start the gateway:

```bash
./omny add groq gsk_...
./omny
```

Send a normal OpenAI chat request:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"fast","messages":[{"role":"user","content":"hi"}]}'
```

With no command, `omny` runs the gateway. Use `-c path/to/omny.yaml` to choose
a different config file.

## Configuration

A small config looks like this:

```yaml
token: replace-me
listen: 127.0.0.1:8080
state: ./omny.state.json
pid: ./omny.pid
default: groq
fallback: [fast]

providers:
  groq:
    url: https://api.groq.com/openai/v1
    keys: [gsk_...]
  ollama:
    url: http://127.0.0.1:11434/v1
    keyless: true

aliases:
  fast: [groq/llama-3.3-70b-versatile, ollama/qwen3]
```

- `keys: []` disables a provider. `keyless: true` sends no upstream bearer
  header.
- An alias is an ordered failover list. In `provider/model`, only the first
  slash separates the provider name from its model.
- `default` handles an unknown model name. `fallback` lists aliases to try
  after the requested route is exhausted.
- `rpm` and `rpd` reserve per-key request capacity. `0` means no configured
  limit.
- `ttft` raises the time allowed for a slow first token. It never lowers the
  default.

The loader rejects invalid config, unknown fields, empty tokens, duplicate
keys, wildcard binds, and config files that are not mode `0600` on Unix.
Windows uses ACLs instead of Unix mode bits.

`omny.example.yaml` contains provider URLs and example aliases. Free provider
plans and model lists change, so check each provider before relying on it.

## Commands

```text
omny add <provider> [key]   add a key; no key or '-' reads stdin
    -url <base>             add a new OpenAI-compatible provider
    -model <id>             make that model available as <provider>
    -keyless                add the provider without a key
omny rm <provider> <key>    remove a key or its /status hash prefix
omny ls                     list providers and key fingerprints
omny check                  validate the config
```

`-url` takes a provider base URL. omny adds `/chat/completions` and `/models`.
`add` and `rm` validate the edited file, then reload a running process through
the configured pidfile.

## Request handling

- All endpoints require `Authorization: Bearer <token>`, including `/healthz`
  and `/readyz`.
- omny waits for a real first token before sending a response to the client.
  A provider error hidden inside HTTP 200 can therefore trigger failover.
- Streaming responses are relayed as they arrive. A client disconnect stops
  the upstream request.
- Request bodies are limited to 32 MB. In-flight bodies are limited to 256 MB.
- Keys use cooldowns after failures. `401` and `403` disable a key until the
  next restart. `Retry-After` takes precedence over the cooldown ladder.
- SIGHUP reloads a valid config without dropping the listener. SIGTERM drains
  active streams before exit.

## Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/chat/completions` | OpenAI-compatible chat relay |
| `GET` | `/v1/models` | configured aliases and provider/model pins |
| `GET` | `/status` | key state, cooldowns, and quota counters |
| `GET` | `/status?probe=models` | check provider model endpoints |
| `GET` | `/status?probe=chat` | send one probe request per key |
| `GET` | `/usage` | token counts and measured first-token times |
| `GET` | `/healthz` | process liveness |
| `GET` | `/readyz` | whether any key can serve now |

The two probe modes use the config snapshot and do not bench routing keys.
The chat probe does spend one upstream request per key.

## Deployment

Keep omny on loopback, a private address, or behind a trusted private network.
omny does not terminate TLS. For public access, put a TLS reverse proxy in
front of it and proxy to omny on loopback.

The config contains API keys. Keep it at mode `0600`, do not commit it, and
limit access to the state file and logs. The state file stores key fingerprints
and daily counters, not the keys themselves. See [SECURITY.md](SECURITY.md).

## Development

```bash
make test
make lint
make fmt
./scripts/e2e.sh -offline
make dist
```

The project uses Go 1.26 and one dependency, `gopkg.in/yaml.v3`.

## License

MIT. See [LICENSE](LICENSE).
