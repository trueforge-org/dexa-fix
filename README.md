<p align="center">
  <img src="pkg/frontend/static/favicon.svg" width="96" alt="DexaFix logo">
</p>

<h1 align="center">DexaFix</h1>

<p align="center">
  <em>Corrects smart-scale results using DEXA scan results.</em>
</p>

<p align="center">

[![CI](https://github.com/trueforge-org/dexafix/actions/workflows/ci.yaml/badge.svg)](https://github.com/trueforge-org/dexafix/actions/workflows/ci.yaml)
[![codecov](https://codecov.io/gh/trueforge-org/dexafix/graph/badge.svg)](https://codecov.io/gh/trueforge-org/dexafix)
[![Latest release](https://img.shields.io/github/v/release/trueforge-org/dexafix)](https://github.com/trueforge-org/dexafix/releases/latest)
[![License](https://img.shields.io/github/license/trueforge-org/dexafix)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/trueforge-org/dexafix)](go.mod)
[![Container image](https://img.shields.io/badge/ghcr.io-dexa--fix-2496ED?logo=docker&logoColor=white)](https://github.com/trueforge-org/dexafix/pkgs/container/dexafix)

</p>

<p align="center">
  <a href="#-quick-start">Quick start</a> &nbsp;·&nbsp;
  <a href="#-features">Features</a> &nbsp;·&nbsp;
  <a href="docs/index.md">Documentation</a> &nbsp;·&nbsp;
  <a href="docs/02-configuration.md">Configuration</a> &nbsp;·&nbsp;
  <a href="#-security">Security</a> &nbsp;·&nbsp;
  <a href="#-contributing-and-development">Contributing</a>
</p>

---

DexaFix is a Go web app that stores per-user DEXA scans and scale readings, and
shows one estimated value per day. Each user has an account and sees only their
own data. Users sign in with a password or with an external provider.

> [!WARNING]
> Read [Limitations and security](docs/14-limitations-and-security.md) before you
> expose DexaFix to the internet.

## ✨ Features

| Area | Feature |
| --- | --- |
| 📊 **Measurements and estimates** | Per-user DEXA scans and scale readings, entered in the browser or by JSON API. |
| | Each reading records its measurement method (smart scale, caliper, BodPod, hydrostatic, tape, visual AI) and whether athlete mode was on. |
| | One estimate per day for weight, body fat, fat mass, fat-free mass, lean mass, hydration, bone mass, visceral fat index, BMI, fat mass index (FMI), lean mass index (LMI) and the android/gynoid (A/G) fat ratio. |
| 🔐 **Accounts and security** | Password sign-in, plus OpenID Connect, Sign in with Apple and OAuth 2.0 providers. See [Accounts and sign-in](docs/07-accounts-and-sign-in.md). |
| | Passkeys (WebAuthn) for signing in without a password, with a device's fingerprint, face or PIN. See [Passkeys](docs/07-accounts-and-sign-in.md#passkeys). |
| | Password reset by one-time link, made by an administrator or requested by e-mail. See [Password reset](docs/07-accounts-and-sign-in.md#password-reset). |
| | Optional two-step verification for password sign-in, with an authenticator app and recovery codes, and an administrator policy for who must use it. Administrators confirm a code again before each Admin page. |
| | New accounts are closed by default, and security-relevant events go to an audit log. |
| 📥 **Imports** | Optional Withings import, so scale readings do not need typing in. See [Withings import](docs/10-withings-import.md). |
| | Strength Level sync, per day, with multi-day sync for users an administrator allows. |
| 🗂️ **Data** | Export of settings and personal details, and of all scans and readings, as JSON (GDPR articles 15 and 20). See [Your data](docs/07-accounts-and-sign-in.md#your-data). |
| | Deleting a user removes their measurements and settings. |
| 🛠️ **Operations** | SQLite (default) or Postgres. Schema changes are versioned migrations, applied on startup. See [Database](docs/04-database.md). |
| | Server settings (base URL, trusted proxies, PROXY protocol, audit retention, e-mail) are edited on the Admin page and take effect at once, without a restart. |
| | Optional HTTPS on its own port, with HTTP/2 and HTTP/3 (QUIC), using an uploaded or a self-signed certificate. See [Running behind a reverse proxy](docs/03-reverse-proxy.md#https-http2-and-http3). |
| | Container image on a `scratch` base, for amd64 and arm64. |
| | Signed images and checksums (cosign), published by the [release workflow](docs/12-ci-and-releases.md). |

## 🚀 Quick start

Requires the Go version in [`go.mod`](go.mod).

```bash
go run . serve
```

Open <http://localhost:8080> and create an account. The first account becomes
the administrator.

> [!IMPORTANT]
> Unless `DEXAFIX_ENCRYPTION_KEY` is set, the first run creates the encryption
> key file `dexafix.key`. Back it up: the server does not start without it.

Or run the container image. The key file is then inside the data volume, at
`/config/dexafix.key`:

```bash
docker run -p 8080:8080 -v dexafix-data:/config ghcr.io/trueforge-org/dexafix:latest
```

Continue with [Getting started](docs/01-getting-started.md).

## 🧩 Running as two processes

`dexafix serve web` runs the pages and the public listeners, and
`dexafix serve api` runs the database and the security decisions. Each can be
deployed separately. See [Split deployment](docs/15-split-deployment.md).

## 📚 Documentation

The full documentation starts at [docs/index.md](docs/index.md).

| Page | What you find there |
| --- | --- |
| [01 · Getting started](docs/01-getting-started.md) | Build, run and create the first account. |
| [02 · Configuration](docs/02-configuration.md) | Flags, environment variables and secrets. |
| [03 · Running behind a reverse proxy](docs/03-reverse-proxy.md) | Trusted proxies, base URL, PROXY protocol, and HTTPS with HTTP/2 and HTTP/3. |
| [04 · Database](docs/04-database.md) | SQLite and Postgres, migrations and moving between them. |
| [05 · Commands](docs/05-commands.md) | The `db` commands for migrations, copies and rekeying, and recovery commands. |
| [06 · Using the app](docs/06-using-the-app.md) | The pages of the web interface. |
| [07 · Accounts and sign-in](docs/07-accounts-and-sign-in.md) | Providers, passkeys, password reset, two-step verification, audit log and your data. |
| [08 · Encryption](docs/08-encryption.md) | What is encrypted, the key and rotating it. |
| [09 · Estimates](docs/09-estimates.md) | How the daily estimates are calculated. |
| [10 · Withings import](docs/10-withings-import.md) | Linking Withings and importing readings. |
| [11 · JSON API](docs/11-api.md) | Routes, authentication and the import endpoint. |
| [12 · CI and releases](docs/12-ci-and-releases.md) | What CI checks and how releases are made. |
| [13 · Development](docs/13-development.md) | Tests, project layout and schema changes. |
| [14 · Limitations and security](docs/14-limitations-and-security.md) | Read before you expose the app. |
| [15 · Split deployment](docs/15-split-deployment.md) | Running the frontend and the backend as two processes, with Compose and troubleshooting. |
| [16 · Workers](docs/16-workers.md) | The processes that talk to Withings and Strength Level, and the Workers admin page. |
| [17 · Body service](docs/17-body-service.md) | The readings, scans and estimates as a process of their own, with a database, key and secret of their own. |
| [Key rotation](docs/key-rotation.md) | Design note on the key check and why rotation stays manual. |
| [CI details](docs/ci.md) | The README badges, the secrets to configure and how to cut a release. |

## 🔐 Security

- **Encrypted at rest:** e-mail addresses, Withings tokens, the Strength Level
  login, provider secrets, two-step verification secrets and every server
  setting (including the SMTP password and an uploaded HTTPS key) are encrypted
  with AES-256-GCM.
- **Hashed secrets:** passwords are hashed with bcrypt, and session and access
  tokens and recovery codes are stored only as hashes.
- **Optional second step:** two-step verification (authenticator app and
  recovery codes) protects password sign-in. Administrators can require it.
- **Audit log:** sign-ins, account and token changes, imports, exports and every
  administrator action are recorded.

<details>
<summary><b>Read more</b></summary>

- [Encryption](docs/08-encryption.md): what is encrypted, how the key is kept and how to rotate it.
- [Limitations and security](docs/14-limitations-and-security.md): the threat model and what DexaFix does not do.
- [Running behind a reverse proxy](docs/03-reverse-proxy.md): trusted proxies, the PROXY protocol and HTTPS, needed for correct client addresses.

</details>

To report a vulnerability or a bug, open an issue at
[github.com/trueforge-org/dexafix/issues](https://github.com/trueforge-org/dexafix/issues).

## 🤝 Contributing and development

Tests, the project layout and how to make schema changes are in
[Development](docs/13-development.md). CI checks and release builds are described
in [CI and releases](docs/12-ci-and-releases.md).

Ideas and questions are welcome as
[GitHub issues](https://github.com/trueforge-org/dexafix/issues).

## 📄 License

DexaFix is licensed under the GNU Affero General Public License, version 3.
See [LICENSE](LICENSE).
