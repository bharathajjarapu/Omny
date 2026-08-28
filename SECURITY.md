# Security

## Report a vulnerability

Open a [private security advisory](https://github.com/bharathajjarapu/omny/security/advisories/new).
Do not open a public issue if it could expose a key or bypass authentication.

## Deployment

`omny.yaml` contains your upstream API keys.

- Keep the file at mode `0600` on Unix. omny refuses a more permissive file.
- Keep the state file and logs private. The state file contains key fingerprints
  and usage counters, not the keys.
- Bind omny to loopback or a private address. The config rejects wildcard
  binds such as `0.0.0.0` and `::`.
- omny does not provide TLS. Use a TLS reverse proxy before exposing it outside
  a trusted network.

## Authentication

Every endpoint requires a bearer token, including `/healthz` and `/readyz`.
Use `tokens:` when different clients need separate credentials:

```yaml
tokens:
  laptop: "..."
  phone: "..."
```

Token names appear in request logs. Delete one token and reload to revoke that
client. Tokens are not scoped or rate-limited, so every valid token can use
every configured provider and endpoint.

Do not put tokens in URLs, shell history, issues, or copied logs. `omny add`
can read a key from stdin so it does not appear in shell history.

## Data sent upstream

omny forwards chat bodies to the provider selected for the request. Providers
can log or retain prompts and responses under their own policies. Check those
policies before using a provider with private data. Keyless providers may have
additional logging terms. The example config calls out known provider warnings.

omny only changes the model field and, for streaming usage reporting, may add
`stream_options: {"include_usage": true}` when the caller did not provide it.
Unknown request fields pass through to the provider.

## Limits

- The config file is not encrypted. Anyone who can read it can use the keys.
- Tokens have full access. There are no per-client permissions.
- The state file is not encrypted and reveals configured key fingerprints and
  usage amounts.
- Run one instance per state file. Two instances can double-count quota.
- Logs hide configured keys and scrub upstream error bodies, but they are not a
  tamper-proof audit log.

## Scope

In scope:

- leaking an API key through responses, logs, or files
- bypassing bearer authentication
- routing a request to an unintended configured provider
- crashing the process with a request

Out of scope:

- a provider's own rate limits or data policy
- an attacker who already has write access to `omny.yaml`
