# OpenDota fixtures

Real responses recorded once on 2026-10-24 from `https://api.opendota.com/api`
(spec version 31.1.0), then trimmed. Tests serve them through `httptest`
and never hit the network.

| File | Request | Trimmed |
| --- | --- | --- |
| `player_337895673.json` | `GET /players/337895673` | `aliases`, `profile.last_login`, `profile.loccountrycode`, `profile.profileurl` |
| `player_337895673_matches.json` | `GET /players/337895673/matches?significant=0` | Kept the 20 newest entries plus `9017652399` (parsed), in original order |
| `match_9017652399_parsed.json` | `GET /matches/9017652399` | `cosmetics`, `all_word_counts`, `my_word_counts`; per player `cosmetics`, `lane_pos`, `last_login` |
| `match_9056907605_unparsed.json` | `GET /matches/9056907605` | Same as above |
| `player_337895673_matches_projected.json` | `GET /players/337895673/matches?limit=3&significant=0&project=...` with every field in `opendota.PlayerMatchFields` | None |
| `player_not_found.json` | `GET /players/4294967290` (404) | None |
| `constants_heroes.json` | `GET /constants/heroes` | Kept the 25 heroes in the recorded matches and only `id`, `name`, `localized_name`, `img`, `icon` |
| `constants_items.json` | `GET /constants/items` | Kept the 166 items in the recorded matches' slots and purchase logs, and only `id`, `img`, `dname`, `cost` |
| `constants_game_mode.json`, `constants_lobby_type.json`, `constants_patch.json` | `GET /constants/{name}` | None |
| `constants_chat_wheel.json` | `GET /constants/chat_wheel` | Kept the phrases used in the recorded matches' chat |
| `request_post_9056907605.json` | `POST /request/9056907605` | None |
| `request_job_not_queued.json` | `GET /request/{jobId}`, polled for 2 min after both requests | None |
| `request_job_queued.json` | `GET /request/602532531` 1 s after `POST /request/9056857943` | None |
| `match_9056907605_parsed.json` | `GET /matches/9056907605`, about 2 min after the parse request | Same as the other matches |
| `match_2324742574_old.json` | `GET /matches/2324742574` (2016), after a parse request that never landed | Same as the other matches |
| `hero_stats.json` | `GET /heroStats` | Kept heroes 1 and 44 |
| `benchmarks_hero_44.json` | `GET /benchmarks?hero_id=44` | None |

Observations from the recording (trust these over the spec):

- An unparsed match has `"version": null` and `od_data.has_parsed: false`.
  Parsed-only player fields (`lane`, `lane_role`, `is_roaming`, `*_t` arrays,
  logs) are absent, not null.
- Present in responses but missing from the spec: `net_worth`, `networth_t`,
  `item_neutral2`, `od_data`. `skill` is in the spec but absent.
- Per-player `rank_tier` can be null.
- `/players/{id}/matches` defaults to `significant=1`, which hides some
  matches; full history needs `significant=0`.
- Response headers report `x-rate-limit-remaining-minute` (60/min) and
  `x-rate-limit-remaining-day` (3000/day without a key).
- `POST /request/{match_id}` counts as 10 calls for rate limiting (spec).
- `project=` on `/players/{id}/matches` replaces the default fields: only
  `match_id`, `player_slot`, `radiant_win` and the projected fields come
  back. `backpack_*`, `item_neutral2`, `net_worth` and `rank_tier` are
  silently ignored.
- An unknown account: `/players/{id}` is 404 `{"error":"Not Found"}`, and
  `/players/{id}/matches` is 200 `[]`.
- An account with hidden match data still has a profile, with
  `profile.fh_unavailable: true`, and its `/players/{id}/matches` is `[]`,
  even when it shows up with its account ID in other players' matches.
- `/constants/heroes` is keyed by hero ID, `/constants/items` by item name
  (`blink`); `game_mode` and `lobby_type` by ID; `patch` is an array.
  `dname` is missing for a few items. `item_neutral2` holds the neutral
  enhancement (e.g. `enhancement_alert`).
- `img` and `icon` paths resolve against `https://cdn.cloudflare.steamstatic.com`.
- `POST /request/{id}` answers `{"job":{"jobId":<number>}}`. While the job is
  queued or running, `GET /request/{jobId}` returns the job object
  (`request_job_queued.json`, seen for about 4 s); afterwards it returns
  `null`, whether the parse worked or not. Neither endpoint reports a
  failure reason, so a job that left the queue while the match is still
  unparsed is the only sign of an expired replay. 9056857943 was parsed
  about 45 s after its request.
- 9056907605 went from `version: null` to `version: 22` within 2 minutes.
- The 2016 match has no `version` key at all, and its players have no
  `hero_damage`, `tower_damage` or `hero_healing`.
- Parsed players: `gold_t`, `xp_t`, `lh_t`, `dn_t` are per-minute arrays;
  `purchase_log` entries are `{time, key}` and the same item can appear
  twice at the same second; ward logs (`obs_log`, `sen_log`) and their
  `*_left_log` share an `ehandle`; `benchmarks` is `{metric: {raw, pct}}`.
- `/heroStats` entries carry `1_pick`..`8_win` per rank bracket plus
  `pub_*`, `turbo_*` and `pro_*` counts; `/benchmarks` is
  `{hero_id, result: {metric: [{percentile, value}]}}`.
