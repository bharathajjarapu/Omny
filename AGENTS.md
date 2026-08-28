# AGENTS.md — omny

**omny** is a single-binary, self-hosted LLM gateway. It pools free-tier API keys across many
providers, exposes them as one OpenAI-compatible endpoint, and fails over
automatically when a key or provider rate-limits.

The goal is a gateway that routes and switches with minimal added latency and
stays reliable through a long streaming session. Everything below serves that.

## Where the reasoning lives

**In the code, on the line it explains.** There is no separate decision record — the
comment beside a branch is the record, and it says *why*, because the code already says
what. A choice that cannot be defended in one line beside the thing it governs is a
choice that has not been understood yet.

That puts a duty on edits: changing behaviour means changing the comment that justified
the old behaviour in the same diff. A comment left standing over code that no longer
matches it is worse than no comment, because it is confidently wrong.

Go module `omny`, binary `omny`, config `omny.yaml`, state `omny.state.json`, pidfile
`omny.pid`. The binary is also the admin tool: `omny add|rm|ls|check`.

## Naming

**One word. Two when one lies.** A name says what the thing is, not which category
it belongs to. Scope sets the budget: a loop variable is one letter, an exported
type may be two words.

| Write | Instead of |
|---|---|
| `pick(alias)` | `selectProviderForAlias()` |
| `bench(key)` | `markKeyAsUnavailable()` |
| `pool.go` | `key_pool_manager.go` |
| `k`, `p`, `n` | `keyIndex`, `providerObj`, `numRetries` |
| `Key.until` | `Key.cooldownExpiresAtTime` |

Words that describe code rather than the domain — `Manager`, `Handler`, `Service`,
`Helper`, `Util`, `Info`, `Data` — mean the name has not been found yet. Keep
looking until it is a noun from the problem: `pool`, `alias`, `route`, `stream`,
`quota`, `bench`.

## Code

**Minimal.** Ship the shortest thing that works. Reach for the stdlib before
writing code, and for code before adding a dependency. The project has exactly
one dependency, `gopkg.in/yaml.v3` — a second one is a decision, so it gets argued in
the pull request before it goes into `go.mod`. Build an abstraction on its
second caller, not its first.

**Flat, one level down.** `cmd/omny` parses flags. `internal/omny` is the gateway — one
package, one file per concern, `run.go` for wiring. `internal/edit` is `omny add|rm|ls|
check`. Nothing subdivides further: the pool, the walk, the gate and the relay read each
other's unexported state, and splitting them into `config/ pool/ gateway/` would export
forty identifiers to buy nothing.

**A package is a seam that has already shown itself, and the export list is the proof.**
`internal/edit` earns its own directory because it shares exactly five names with the
gateway — `Config`, `Load`, `Parse`, `Fingerprint`, `Replace` — and does unrelated work:
YAML line surgery, not proxying. Count first. A split that needs a page of exports to
compile is not a seam, it is the same organism in two rooms.

The boundary also buys something the flat layout could only promise. `omny add` validates
its edit with `omny.Load` — the server's own loader — and across a package line the
compiler is what enforces that there is no second validator to drift. A rule that used to
live in a comment now lives in the import graph.

`Run`, `Load`, `Parse`, `Fingerprint`, `Replace`: five functions leave `internal/omny`,
and each one is named by a caller that exists. A sixth means either the command is
growing logic that belongs in the package, or a new seam deserves the same argument this
one had to make — not a quiet export.

**Opaque.** Read a request only far enough to find `model` and `stream`; forward
every other byte untouched. This is the rule most likely to be eroded by
a well-meaning edit: any struct that models a chat message, any field allowlist,
any "just validate this one parameter" is a regression, and it costs tool calling
and vision support the moment it lands.

**Fast where it counts.** The hot path is bytes in, bytes out. Stream with
`io.Copy` and a reused buffer rather than assembling chunks; keep per-request
allocation off the relay path. Everywhere else, clarity wins — the gateway is
waiting on a network, not on the CPU.

**Honest under failure.** A gateway's whole value is behaving well when an
upstream misbehaves, so malformed bytes, a truncated stream and a provider
returning HTTP 200 with an error body are ordinary inputs, not edge cases. Wrap
errors with `%w` and classify them where the cooldown ladder can act on them.

**Commented in one line.** A comment is a single short line saying *why* — the code
already says what. Write the line a reader needs and stop; a comment that wants a
paragraph means the name is wrong or the function does two things, so fix that
instead. Most code needs none: comment the non-obvious choice, the provider quirk,
the reason a branch exists.

Non-trivial logic — the failover walk, the cooldown ladder, the first-chunk commit
gate — leaves one runnable test behind, using stdlib `testing` and table cases.

## Patterns

The rules above say what the code looks like. These say how it is put together.

**Inject one thing.** Dependency injection here is a struct field with a working
default, not a framework and not an interface per collaborator. `Pool.now` defaults
to `time.Now`; a test replaces it to reach the ladder's 24h rung. That is the only
injection in the codebase, and it stays that way because everything else already has
a seam: a fake provider is an `httptest.Server` URL in the config, since
`providers[].url` is a config field. **An interface that exists so a test can pass is
not a design — it is a test leaking into production.**

**DRY is about knowledge, not about lines.** One fact lives in one place: the cooldown
ladder is one table, the model-name split is one function, the key identity is one
hash. Two blocks that merely rhyme — two validation loops, two header copies — change
for different reasons and are not duplication; merging them invents a shared concept
that does not exist. Abstract on the second *caller* of the same fact, never on the
second *appearance* of the same shape.

**Zero values work.** A `Key` with no cooldown is available; a `Provider` with `rpd: 0`
is unmetered. No constructor exists only to fill in defaults, and `nil` is never a
state the caller must ask about before using the value.

**Errors carry; they do not announce.** Wrap with `%w` at every hop so the cause
survives, classify once where policy can act on it, and log once at the top of
the handler. Logging and returning the same error is two lines in the aggregator for
one event. Upstream detail never reaches the client — it reaches the log line.

**Fail closed.** A config that will not validate stops the process. A token that
matches none of the configured ones is 401 before anything else runs — and an empty
token matches nothing, because `ConstantTimeCompare("", "")` is 1 and `Bearer ` would
otherwise be a credential. A config carrying an empty token is refused at startup. A world-readable keyfile is refused,
not warned about. Every default points at the safe answer: loopback, not
wildcard; benched, not retried blindly.

**Allocate outside the relay.** The hot path is bytes in, bytes out: a `sync.Pool` of
32 KB buffers feeding the copy loop, a body read once and reused across attempts, an
`atomic.Pointer[Config]` read per request instead of a lock. Everywhere else, clarity
wins — the gateway waits on a network, not on the CPU. (Not `io.CopyBuffer`:
`ResponseWriter` implements `ReaderFrom`, so it ignores the buffer and offers no hook to
flush between writes. The loop is hand-rolled for the flush.)

**A budget is not a collaborator.** `Pool.now` is the one injection because a clock is a
*dependency* — it reaches outside the process for an answer. A duration is not: `ttft`,
`idle` and `pause` are struct fields with working defaults, and a test that sets one to
40ms is choosing a value, not substituting a component. The test for the difference is
whether the production default could ever be `nil`. If it could, it is a collaborator and
it needs a real seam; if it cannot, it is a knob, and a knob costs one field.

**Cancellation is the only way to stop a read.** A timer cannot interrupt `io.Read`, and no
deadline field reaches a body already in flight. The single mechanism is to cancel the
context that owns the connection, which makes the pending read return. So the `cancel` from
`context.WithCancel` is a *resource*: it is armed by the first-token timer, disarmed the
moment the gate commits, re-armed by the inter-chunk timer, and released exactly once when
the relay ends. Ownership travels with the value that outlives the function — that is why
`result` carries it — and every path that does not hand it on must call it.

**Persist what re-derives badly.** The state file holds the day's counters and nothing else.
Cooldowns are seconds-to-hours of live state that rebuilds itself on the next failure, so
persisting them would only let a stale file bench a healthy key across a restart. Quota is
the opposite: nothing rediscovers today's spend except spending it again. Write with temp +
`os.Rename`, key it by a hash of the secret rather than its position in the file, and drop
a record whose day does not match — a counter that is silently wrong is worse than absent.

**A cap counted on the way back is not a cap.** `rpd` and `rpm` decide which key `pick`
hands out, so they have to count what is in flight as well as what has come back. Counting
only finished requests means every member of a burst reads the same stale total and every
one of them passes: measured against a live gateway, a provider capped at three served ten
of ten concurrent requests. So `pick` reserves and exactly one of `ok`, `fail` or `done`
releases. That "exactly one" is the whole guarantee, and it is why the client-left path
calls `done` explicitly rather than falling through, and why a relay that breaks after the
gate committed calls `sour` rather than `fail` — `ok` already released that slot, and
releasing it twice would hand the cap a free request.

**Break the guard, watch the test fail.** Non-trivial logic leaves a runnable test behind;
a guard leaves a *verified* one. Delete the branch, invert the comparison, drop the timer
reset — a named test must go red. Two guards in this build passed a test suite that could
not actually see them, and the mutation is what found that, not the review.

**Swap the whole; snapshot the whole.** Live reconfiguration is one `atomic.Pointer[Config]`
store, and a request `Load`s it exactly once, at the top. Both halves matter: swapping field
by field lets a request read half of one config and half of another, and re-loading per hop
lets a reload re-route a walk that is already under way. The new file is parsed and validated
in full *before* anything swaps, so a typo leaves the running gateway serving — `load` is the
same function boot uses, which is why there is no second validator to drift.

**Rebuild derived state; reconcile identity.** A reload rebuilds the pool from the new config
and carries every key forward by **fingerprint**, so a cooldown, a ladder rung and the day's
count survive an edit elsewhere in the file. Anything *derived from the old shape* starts over
instead: the round-robin cursor is an index into a slice the reload may have shortened, and
carrying it forward is a panic waiting for the first key someone removes. Identity is matched;
positions are discarded.

The trap is the middle case — a long-lived object that a reload *mutates in place*. Reusing
the `*Key` is right, because its cooldown is the thing worth keeping; re-pointing its
`*Provider` was wrong, because a request already holding that key would have sent its next
hop to the new address. **What a request must not see change travels on the request**, which
is why the provider address rides on the `target` `route` resolved from the snapshot, and why
what is left on the key — the daily quota — is read only under the pool's lock.

**Redact at the source, not at the call site.** "No log line contains key material" is a
guarantee only if it cannot be forgotten. The scrubber is built from the key list itself, in
the same function that builds the keys, so it cannot drift from what it is protecting — a
provider is free to echo a key back inside a body it answers `200` to, and that body becomes
an error and then a log line. A rule that each `log` call must remember to mask is not a
guarantee, it is a habit. It also never forgets: a key a reload removed is still held by
every request already in flight, so the table is cumulative — dropping an entry the moment
the config drops it would unmask exactly the requests still using it.

**One event, one line, assembled once.** A request has seven ways to end. Seven log calls
means seven copies of the same field list, and the first edit makes them disagree — so the
fields travel as one `trace` the walk fills in, and the line is written in one place. The
converse holds too: a bench and a recovery are separate *events*, so they get their own lines
rather than being folded into the request's.

**A diagnostic must not be able to change what it measures.** The probe reads the config
snapshot, never the pool — which is what makes "a probe cannot bench your keys" true by
construction rather than by remembering. Reading the pool would have been the shorter code
and the wrong one: a transient blip during a probe would cost a healthy key a ladder rung,
and a tool that can disable the thing it inspects is a footgun, not a diagnostic.

**Identity has to differ wherever the thing differs.** A key is identified by a hash of its
secret, which holds until the secret is absent: the hash of "no key" is the same for
everyone, so two keyless providers would have shared one card and one line in the state
file. Identity falls back to the provider's name — and the empty value never reaches the
scrub table at all, because an empty pattern makes `strings.Replacer` insert its
replacement between every rune of every line it touches.

**One budget, honoured everywhere it applies.** A provider that declares a 60s first-token
budget gets it from the probe as well as the relay. Measured live, a cold Gemini answered
in over 20s and the probe reported it *unreachable* — a false alarm invented by a second
timeout that knew nothing about the first. Two call sites reading one fact is DRY; two
call sites each inventing their own is how a diagnostic learns to lie.

**Measure what already goes past.** The commit gate waits for the first token, which means
the gateway has always known how long each provider takes and has always thrown it away.
Recording it is one field and turns a configured order into a measured one — and because
the number is a by-product of work already being done, it costs nothing and cannot itself
be wrong about load. A metric you have to send a request to obtain is a metric that changes
what it measures; one that reads a number already going by is free. The same applies to
token counts: the non-streaming body is already unmarshalled once to catch an error buried
in a 200, so the count comes out of that pass rather than a second one.

**A deadband is quantised, not a tolerance.** "Reorder only when the difference is worth
acting on" cannot be written as `abs(a-b) < eps` in a comparator: that relation is not
transitive, and a sort needs it to be. Rank into tiers — here, doublings of a 250ms floor —
and sort stably, so the buckets do the tolerating and config order decides inside one. Which
is the other half of the rule: **the file's order is the tiebreak, not the fallback.** A
provider written first because its model is preferred must not be demoted over 30ms of noise,
and a target with no measurement at all keeps the index it was given rather than being
guessed at — an invented average either buries a new key forever or promotes it over a
proven one.

**Only ever in the safe direction.** A provider's declared `ttft:` raises the first-token
budget; a measurement lowers it; neither can do the other's job. Stacking two adjustments
that can each push either way makes the result depend on the order they are applied, and
no one will remember what that order is. Give each input one direction and a floor, and the
composition stays obvious: `min(declared, max(floor, 3*measured))` reads the same at every
call site because it can only shrink.

**One request may be bounded; all of them must be too.** `maxBody` caps a single body and
says nothing about how many are in flight, and 32MB times a burst is the only way this
gateway runs out of memory — measured, sixteen concurrent 32MB requests reached 679MB.
So `admit` reserves room *before* the read, from the length the caller declared, and an
unknown length is charged the maximum because that is what it may turn out to be. It is a
reservation for the same reason `pick` reserves: a total of bodies already in memory
cannot refuse the one that would not fit. Bounded, the same burst peaks at the ceiling.

**Liveness is dumb on purpose.** A restart cannot un-bench a key, so a gateway with every
key cooling down is alive and working — and `/healthz` says so, or a supervisor would
restart it in a loop for a condition a restart cannot fix. Readiness is the different
question a load balancer is really asking, so it is `/readyz`, a second endpoint rather
than a smarter first one. Both sit behind the bearer token like every other route: one rule
for the whole server is a rule nobody has to remember an exception to, and a supervisor that
can run a health check can set a header. The cost is a probe that carries a secret; the thing
bought is that "is anything on this port unauthenticated" has one answer, and it is no.

**One id per request, and it must survive a restart.** Seven exit paths and an interleaved
burst of failovers mean "which attempt belongs to which request" has no answer without an
id on every line. A counter alone is not one: logs are appended to across restarts, so the
counter starts over and two unrelated requests both answer to `1`. That is worse than no
id, because it invites a confident wrong answer where none was available — the project's
own e2e misattributed three providers before the run prefix existed. A caller's own
`X-Request-Id` wins, bounded and printable-checked, so the id matches their logs.

**A panic is a 500, not a dropped socket.** `net/http` already recovers one and keeps the
process up, but the caller gets EOF rather than a status, and the line it writes lands at
INFO — a crash logged as information is a crash nobody is paged for. The shield logs at
ERROR with a stack and answers the OpenAI-shaped error every other failure answers with.
`http.ErrAbortHandler` is re-panicked through `errors.Is`, because a deliberate abort is
not a crash.

**Splice, do not re-encode.** The model name is the one thing omny rewrites, so the body
travels as the caller's own bytes with a new name spliced over the old one — located once
by the parser, then handed to the transport as pieces in order. Decoding a body into a map
and re-encoding it costs two full copies of it, per hop, and measured on 8MB requests that
was three quarters of the memory the gateway used. It is also less opaque, not more: a
splice cannot reorder a key, drop a duplicate or renormalise a float, because it never
looks at them. The offsets are verified against the parser's own output before use, and a
shape that cannot be verified falls back to decoding — correctness first, speed second.

**Read at the size you were told.** `io.ReadAll` doubles from 512 bytes, so an 8MB body is
assembled through a 16MB buffer and the discarded halves are most of what a burst of large
requests spends. `Content-Length` is a claim rather than a promise, so it sizes the buffer
and `MaxBytesReader` keeps the last word on the cap. Same bytes, a third of the peak.

**Name the exception and give it a file.** The opaque rule has exactly one exception —
`stream_options`, added so a streaming provider will report its token count. It is one map
entry inside the rewrite that already runs, it never overrides a caller's own, and a provider
that refuses it is retried once without and remembered. What keeps it at one is that
everything omny may read or write in a request body lives in `body.go`: a rule stated in a
document is a rule someone widens by accident, and a rule with its own small file is a rule
whose erosion arrives as a diff you cannot miss.

**Unaccounted, not zero.** A reply that reports no token count is counted as a reply that
reported nothing, never as a reply that cost nothing. The distinction is the same one
`/status` already draws between an unknown daily limit and a limit of zero, and it is the
same reason `ttft_ms` is `null` before the first sample and `0` for a provider that answered
inside a millisecond. A total that is quietly short teaches you to trust it and then misleads
you; one that says how short it might be sends you to look.

**Validate with the loader that will load it.** `omny add` writes the edited config to a
temp file at 0600, runs the server's own `Load()` against **that file**, and only then
renames it into place. Not a second validator that agrees today — the same function, on the
same bytes, in the same mode, so a config the server would refuse cannot replace one it
accepts. This is why `Replace()` grew a hook rather than a copy: the state file and the
config file both want temp-write-rename, and only one of them wants an opinion about what
it just wrote.

**Edit the line; parse to find it.** `omny.yaml` is mostly comments, aligned into columns,
and a `yaml.Node` round trip keeps every comment while destroying every column. So the parse
locates the entry — a regular expression over YAML would be guessing — and a line rewrite
changes it, leaving every other byte exactly as it was written. The shapes that cannot be
line-edited, like a provider compacted onto one line, are refused with a message saying so
rather than mangled: a tool that edits the file holding every key you own should fail
loudly and change nothing.

**An insertion moves every line after it.** A custom provider is three edits to one file
— the block, its key, its alias — and they cannot share a parse: the moment the first one
inserts a line, every `yaml.Node` line number past it is off by one, and the second edit
lands somewhere it was never meant to. So an edit is a `tweak`, `amend` applies them in
turn, and it re-parses between. Parsing is cheap; two sets of offsets that have to be kept
in agreement are not.

**The CLI does not get its own vocabulary.** `omny add -model` names a model by writing
an *alias*, not by adding a `model:` field to the provider — "which model does this name
mean" is a question the alias table already answers, and a second mechanism for one fact
is how the two learn to disagree. The whole feature is four tweaks and three flags because
everything it needs was already a config concept; the day it needs a fifth, the question
to ask first is which existing one it is really asking for.

**Compose at the edges.** `run.go` wires and `cmd/omny` only parses flags; nothing below
either constructs its own dependencies or reads a global. A function that reaches for
package state cannot be tested through the seam, which is the same thing as saying it
cannot be tested.

## Skills

`.agents/skills/` holds nine Go skills covering concurrency, context, error
handling, safety, security, observability, CLI, testing and project layout.
Invoke the relevant one rather than working from memory; they carry the current
idioms for exactly the parts of this build that are easy to get subtly wrong.
