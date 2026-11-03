# discord-rss

Polls RSS/Atom feeds and posts new items to Discord webhooks.
Slim Go rewrite of [3ventic/discord-rss](https://github.com/3ventic/discord-rss): no UI, no HTTP server, no Redis.

## Setup

```sh
mkdir -p data
cp examples/config.yaml data/config.yaml   # then edit it
chown -R 65532:65532 data                  # distroless nonroot user
docker compose up -d --build
docker compose logs -f
```

## Files

| File | Purpose |
| --- | --- |
| `data/config.yaml` | Feeds and poll interval, edited by hand, re-read on every poll. See `examples/config.yaml`. |
| `data/state.json` | Timestamp of the last posted item per feed URL, managed by the app. See `examples/state.json`. |

Paths can be overridden with the `CONFIG` and `STATE` environment variables.

## Behaviour

- A newly added feed posts nothing on its first poll, it only records the current position.
  To backfill, put an older timestamp for its URL into `data/state.json`.
- New items are posted oldest first, batched to Discord's limits (10 embeds, 6000 characters per message).
- If posting fails, the state is not advanced and the items are retried on the next poll.
