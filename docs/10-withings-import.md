# Withings import

Each user links their own Withings account. Imported measurements are stored
as scale readings with the source `withings`.

## Setup

1. In the Withings developer dashboard, create an application and register the
   callback URL, for example
   `https://dexa.example.com/integrations/withings/callback`.
2. As an administrator, enter the application's client id and secret in the
   Withings application section of the Admin page. They are stored encrypted in
   the database. The redirect URL defaults to this server's own callback
   address, so set it explicitly only if the address Withings must use differs
   (for example behind a proxy without a base URL on the Admin page). Connecting is possible
   only when all three are set; until then the Settings page shows Withings as
   not set up. Withings is configured only on the Admin page: the old
   `DEXAFIX_WITHINGS_*` variables and flags no longer exist and are ignored with
   a warning.
3. On the estimates or Settings page, choose **Connect Withings** and grant
   access. Finish within 10 minutes, the lifetime of the connection state.
4. Choose **Import from Withings**.

**Disconnect Withings** forgets the user's tokens. Imported readings are kept.

Imported: weight, fat ratio, fat mass, fat-free mass, muscle mass, hydration,
bone mass and visceral fat. Measurements Withings marks as possibly belonging
to someone else (for example a guest) are ignored. Days are calendar days in the
user's time zone.

## Bulk import

**Import from Withings** fetches the whole history. Each day is handled by what
is already stored:

| Day has                | What happens                                                                                          |
| ---------------------- | ----------------------------------------------------------------------------------------------------- |
| No readings            | Imported first.                                                                                       |
| Withings readings only | Changed readings are updated, identical ones are left alone, new ones are added. Nothing deleted.     |
| Manual readings        | New Withings readings are skipped and the day is listed in the result. Earlier imports are refreshed. |

A manual entry is a deliberate value. Averaging it with a Withings reading of
the same weigh-in would distort the day, so bulk import leaves such days alone.

## Re-import one day

Use **↻** on a row of the estimates page, or `?day=` on the API, to re-import
one day. It replaces that day's Withings data:

- Adds the day's Withings readings, even when manual readings exist.
- Updates changed readings.
- Removes stored Withings readings that Withings no longer returns, for
  example after a weigh-in was deleted in the Withings app.
- Never touches manual readings.
