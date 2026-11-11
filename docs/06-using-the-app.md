# Using the app

Settings and Admin are in a menu under your first name (or your username, when
you have not set one) in the header, which also has **Sign out**.

- `/`: the sign-in page. Sign in with an e-mail address and password, with a
  passkey (**Sign in with a passkey**), create an account, or use a provider
  button. A username is unique (ignoring case) and has 2 to 32 letters, digits,
  `.`, `_` or `-`, starting with a letter or digit. You sign in with your
  e-mail address, not your username.
- `/u/{id}`: redirects to the estimates page.
- `/u/{id}/estimates`: one row per day with the estimated values, an overview
  with the latest values and a trend chart, the calibrations in use, and the
  Withings buttons.
- `/u/{id}/dexa`: add and delete DEXA scans. A scan needs a date and at least
  one measurement.
- `/u/{id}/scale`: add and delete readings. Times are in your time zone (Settings). Choose the
  measurement method in the drop-down and tick **Athlete mode** if it applies.
- `/u/{id}/settings`: the athlete-mode default and the accuracy rank of each
  method. The ranking is a drag-to-reorder list, with up and down buttons for
  keyboard use. Without JavaScript it has rank number inputs instead. A Withings
  section connects, imports from and disconnects the user's Withings account. It
  says so when Withings credentials are missing, either on the Admin page or in
  the environment. Also:
  - **Profile**: username, first name and last name (both optional, up to 64
    characters), e-mail, time zone and height in cm. The e-mail is
    required, because it is how you sign in; changing it asks for your current
    password. The time zone (an IANA name such as `Europe/Amsterdam`, default
    `UTC`) decides which calendar day a scale reading belongs to, how the date
    and time of a reading are read and shown, what "today" is and which day a
    Withings import covers. DEXA scans are plain dates and are not moved.
  - **Access tokens**: create and revoke personal access tokens for the API.
  - **Linked accounts**: link or unlink sign-in providers.
  - **Two-step verification**: turn it on or off, and replace your recovery codes.
    See [Two-step verification](07-accounts-and-sign-in.md#two-step-verification).
  - **Passkeys**: add a passkey, rename it or remove it. See
    [Passkeys](07-accounts-and-sign-in.md#passkeys).
  - **Password**: set or change the account's password. Changing it signs out
    the other sessions and revokes all access tokens.
- `/admin`: administrators only. Administrators can add users and open or
  close new accounts. The page shows the server status and has these
  sections:
  - **Users**: grant or remove administrator, set a password, make a one-time
    password reset link, remove a user's two-step verification, or delete a user
    with all their data. The last administrator cannot be removed.
  - **Password sign-in**: turn it on or off.
  - **Two-step verification**: who must use it: nobody, administrators or everyone.
  - **Sign-in providers**: add, edit and remove providers.
  - **Withings application**: the client ID, client secret and redirect URL.
  - **Network and retention**: the base URL, trusted proxies, the PROXY protocol,
    whether sign-in providers and the mail server may be on private network
    addresses, how long the audit log is kept and the encryption key reminder. See
    [Settings on the Admin page](02-configuration.md#settings-on-the-admin-page).
  - **E-mail**: the SMTP server for password reset e-mails, and a test e-mail.
  - **HTTPS, HTTP/2 and HTTP/3**: an optional HTTPS listener with a certificate.
    See [Running behind a reverse proxy](03-reverse-proxy.md#https-http2-and-http3).

Numbers may use a decimal comma. Notes are limited to 500 characters.

While a scale reading or DEXA scan is being entered, an empty field shows a gray
value when it can be calculated from what was typed. Weight, body fat %, fat
mass and fat-free mass calculate from each other: any two give the rest. On the
DEXA form, lean mass + bone mass gives fat-free mass. A typed value is never
replaced. Gray values are saved with the form unless a value is typed instead.
This needs JavaScript; without it the fields stay empty.

On the estimates page, a `DEXA` badge marks a value from a DEXA scan, and a
`corr.` badge marks a corrected value. Faded values are interpolated or held,
and a gray `est` tag in front of the date marks a row with a value
interpolated between two readings of that field. Hovering a value shows its
origin, the method and mode it is based on, and the raw reading. The
calibrations section lists each scan's offsets (DEXA minus the reading on the
scan day) per method and mode. Days with only a DEXA scan appear too. The
daily table shows the same period as the chart (30 days, 90 days, 1 year or
all time, 90 days by default). The Columns menu above the daily table shows
and hides columns, and the choice is remembered per account.

## The footer, terms and privacy

Every page has a footer with links to the documentation, the status page, the
**Terms of Service** (`/terms`), the **Privacy Policy** (`/privacy`), the
project's sponsor page on [Open Collective](https://opencollective.com/dexafix)
and, as a GitHub icon, the source code (which the AGPL asks a server to offer).
Signed-in people see a short, dismissible reminder above the estimates that the
project lives on support; closing it is remembered for 90 days in a cookie.
