# Estimates

## How it works

A smart scale is convenient but biased. A DEXA scan is accurate but rare. The
app uses the difference between the two (the offset) to correct the scale
between scans.

- Stored readings are never changed. Everything below is calculated from them
  and kept in a memory cache until the data or the date changes (see
  [Estimate cache](#estimate-cache)).
- First the app builds its own per-day series: the most accurate reading of
  each day, averaged over a centred 7-day window of real readings, with the
  gaps filled in by straight lines.
- The offset is `DEXA - the actual reading` on the day of each DEXA scan. The
  reading is the scale's own value that day, not the smoothed series.
- On a day with a DEXA value, that value is used.
- On every other day, the series is corrected by the nearby offset.

Example: one DEXA scan on 2026-03-01 with body fat 18.0 %, while the scale says
20.0 % every day. The reading on the scan day is 20.0 %, so the offset is
-2.0 percentage points.

| Date       | Scale (%) | Estimate (%) | Origin                   |
| ---------- | --------- | ------------ | ------------------------ |
| 2026-03-01 | 20.0      | 18.0         | `dexa`                   |
| 2026-03-02 | 20.0      | 18.0         | `measured`               |
| 2026-03-05 | none      | 18.0         | `interpolated` or `held` |

Fat mass and fat-free mass are then calculated from the body fat % and the
weight on every day with a weight, whether or not a calibration applies. The
scale's own values are used only when they cannot be calculated.

The full rules are in [Estimation details](#estimation-details).

## Estimation details

Estimates are computed from the stored data. The result is cached per user and
recalculated when the data or the date changes (see
[Estimate cache](#estimate-cache)). Stored readings are never changed. Each
field is handled on its own.

1. **Daily average.** Readings are averaged per day (a calendar day in the user's time zone), for each field and each
   group. A group is a measurement method plus athlete mode. Missing values are
   ignored.
2. **Most accurate reading.** Each day uses the group whose method ranks
   highest for that field. The default order, most accurate first, is
   hydrostatic, BodPod, caliper, smart scale, tape, visual AI, typed into an
   app (`manual-entry`). Change it on the
   settings page. A method without a value for the field is skipped, so a
   caliper reading with only body fat does not hide the scale weight. If the
   normal and athlete group of one method both have a value, the one matching
   the user's athlete-mode default wins.
3. **Rolling average.** Each day is averaged over the real readings within 3
   days before and after it (a 7-day window, shorter at the ends of the series).
   Only real readings count, never days filled in by step 4. If the window has
   at least 3 days with a real reading, the day's value is that average. This
   also applies to a day without a reading of its own, and its origin stays
   `interpolated` or `held`. Otherwise a day with a reading keeps that reading.
   Peaks are not capped.
4. **Missing days.** A day still without a value from step 3 gets a value on a
   straight line between the nearest days that have one (origin `interpolated`).
   After the last such day, its value is repeated up to today (origin `held`).
   The held value can therefore be the last average, not the last raw reading.
   This is the per-day series.
5. **Calibration.** On each DEXA day inside the series, the offset (DEXA minus
   the reading on that day) is recorded for body fat % and bone mass. Every
   group (method and athlete mode) with a reading of that field on the scan day
   gets its own offset. Several readings of one group on one day are averaged
   first. A group without a reading on the scan day gets no offset from that
   scan. A scan with fat mass and weight but no body fat % gets its body fat %
   calculated as fat mass / weight x 100, so it still calibrates body fat %.
   Weight is measured by DEXA but is never corrected. Hydration and visceral
   fat index are never corrected either, because DEXA does not measure them
   here. A scan with no reading of a group on that day cannot calibrate that
   group.
6. **Estimate.** For each field and day:
   - A DEXA value is used as it is (origin `dexa`).
   - Before the first calibration of the day's group the series is corrected
     with that first calibration's offset.
   - Between two calibrations of the group the offset is blended with a
     smoothstep curve, `w(t) = t²(3 − 2t)`, where `t` is 0 at the earlier scan
     and 1 at the later one: `offset = (1 − w) × earlier + w × later`. The first
     half stays near the earlier scan, and the second half moves towards the
     later one.
   - After the last calibration the last offset is kept.
   - A window that mixes methods applies each reading's own offset: the offsets
     of the groups behind the window's real readings are averaged, weighted by
     how many readings each group has in the window. A group without a
     calibration counts as 0.
   - Corrected values have `corrected: true`. Body fat % is kept within 0 to 100
     and bone mass is kept at 0 or more, after the offset is applied.
   - Fat mass and fat-free mass are calculated on every day with a weight,
     whether or not a calibration applies. When body fat % is known, fat mass =
     weight x body fat % / 100. When it is not, the fat mass already on that day
     is kept. Fat-free mass = weight - fat mass, so the two always add up to the
     weight. The scale's own fat mass and fat-free mass are used only when they
     cannot be calculated, for example on a day with no weight. A DEXA fat mass
     or fat-free mass value on its own day is used as it is, and the other is
     calculated from it where needed. They are marked `corrected: true` when
     the value they come from is corrected or from DEXA. They are unmarked only
     for a group with no calibration at all. Lean mass = fat-free mass - bone
     mass, calculated on every day with both. On a day missing either one there
     is no lean mass. A DEXA lean mass value on its own day is used as it is
     (origin `dexa`). Lean mass is marked `corrected: true` when the fat-free
     mass or bone mass it comes from is corrected or from DEXA.
   - BMI = weight (kg) / height (m)², FMI = fat mass (kg) / height (m)² and
     LMI = lean mass (kg) / height (m)², where lean mass is the fat-free mass
     minus the bone mass. Height comes from the profile in cm. Without a height,
     these three are left out. They are marked `corrected: true` when the value
     they come from is corrected or from DEXA. The A/G ratio = android fat % /
     gynoid fat %, from DEXA scans that have both values (origin `dexa` on the
     scan day). After a scan, the latest ratio is repeated (origin `held`)
     until the next scan with a ratio. There is no value before the first such
     scan.

### Estimate cache

Computing the estimates walks a user's whole history, so the result is kept in
memory per user (for the 32 most recently used users), by the
[body service](17-body-service.md) that computes it. It is reused until
something it depends on changes, and there is no list of places that have to
remember to clear it. The cache is tied to three things:

- the user's body version, a counter in the database that every change to the
  user's scans and readings (web, API, imports) increments in the same
  transaction;
- the settings the estimates depend on: time zone, height, method ranking and
  athlete default, which the api process sends with every call;
- the date, since the series runs up to today.

Because the counter lives in the database, a change made by another process or
server sharing a Postgres database is seen at once as well. The cache starts
empty after a restart or upgrade.

`GET /api/v1/users/{id}/estimates` sends an `ETag`. A client that sends it back
in `If-None-Match` gets `304 Not Modified` while nothing changed, without the
server computing or sending anything, which suits dashboards that poll.
