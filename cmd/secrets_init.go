package cmd

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/trueforge-org/dexa-fix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexa-fix/pkg/shared/mtls"
)

// `dexa-fix secrets init` makes everything the Compose deployment (deploy/docker-compose.yaml)
// needs: the database passwords and connection strings, the encryption keys, the shared secrets of
// the three doors (web, workers, body), the token signing key pair and the mutual TLS certificates
// of both internal links. It replaced the shell script that used to do this, and runs as the `init` container.
//
// What it makes falls into three groups:
//
//   - Kept for good (--host-dir): the database passwords, the connection strings and the two
//     encryption keys. They are created only when missing, because losing an encryption key makes
//     the data unreadable. They live on the host, so `docker compose down -v` cannot delete them.
//   - Kept once (the per-process directories): the service, worker and body secrets and the body
//     token signing key. The running processes read them at start, so they are not changed behind
//     their back. The api directory is the source of truth; the other directories get a copy.
//   - Made again at every run: the mutual TLS certificates. The CA's key is thrown away at once, so
//     a stolen leaf cannot be renewed and the whole set can be replaced by running init again; the
//     processes read changed files within 30 seconds.
//
// A file is replaced atomically (written beside it, then renamed), so a process never reads half a
// file.

// Names of the files in the per-process directories. The Compose file points the *_FILE variables
// at them.
const (
	fileServiceSecret = "service_secret"
	fileWorkerSecret  = "worker_secret"
	fileBodySecret    = "body_secret"
	fileBodyTokenKey  = "body_token_key"
	fileBodyTokenPub  = "body_token_pub"
	fileValkeyURL     = "valkey_url"
	fileMTLSCA        = "mtls_ca"
	fileMTLSCert      = "mtls_cert"
	fileMTLSKey       = "mtls_key"
	fileBodyMTLSCA    = "body_mtls_ca"
	fileBodyMTLSCert  = "body_mtls_cert"
	fileBodyMTLSKey   = "body_mtls_key"
)

type initOptions struct {
	hostDir                            string // persistent secrets, to back up
	apiDir, webDir, workerDir, bodyDir string // one per process; empty skips that process
	dbHost, valkeyHost                 string
	apiNames, bodyNames                []string
	validity                           time.Duration
	uid, gid                           int // owner of the files in the process directories; -1 leaves them
	log                                io.Writer
}

func (o initOptions) say(format string, a ...any) {
	if o.log != nil {
		fmt.Fprintf(o.log, format+"\n", a...)
	}
}

// writeAtomic puts content in dir/name with the mode (and owner, if set), replacing what is there
// without ever leaving a half-written file.
func (o initOptions) writeAtomic(dir, name, content string, mode os.FileMode, chown bool) error {
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op after the rename
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if chown && o.uid >= 0 {
		if err := os.Chown(tmp.Name(), o.uid, o.gid); err != nil {
			return fmt.Errorf("give %s to %d:%d: %w", name, o.uid, o.gid, err)
		}
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// existing returns the trimmed content of dir/name, or "" if there is no such file.
func existing(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// keep returns the value in dir/name, making it with gen first if there is none. host files are
// readable by everyone (the containers run as other users and a bind-mounted secret keeps the
// host's mode; the directory stays private); the others are for their owner only.
func (o initOptions) keep(dir, name string, host bool, gen func() (string, error)) (string, error) {
	v, err := existing(dir, name)
	if err != nil {
		return "", err
	}
	if v != "" {
		o.say("kept      %s", filepath.Join(dir, name))
		return v, nil
	}
	if v, err = gen(); err != nil {
		return "", err
	}
	mode := os.FileMode(0o400)
	if host {
		mode = 0o444
	}
	if err := o.writeAtomic(dir, name, v+"\n", mode, !host); err != nil {
		return "", err
	}
	o.say("created   %s", filepath.Join(dir, name))
	return v, nil
}

func (o initOptions) put(dir, name, content string) error {
	if err := o.writeAtomic(dir, name, strings.TrimSpace(content)+"\n", 0o400, true); err != nil {
		return err
	}
	o.say("replaced  %s", filepath.Join(dir, name))
	return nil
}

func (o initOptions) run() error {
	if o.hostDir == "" {
		return errors.New("--host-dir is required: the directory for the passwords and keys you must back up")
	}
	others := o.webDir != "" || o.workerDir != "" || o.bodyDir != ""
	if others && o.apiDir == "" {
		return errors.New("--api-dir is required with the other process directories: it holds the secrets they share")
	}
	for _, d := range []string{o.hostDir, o.apiDir, o.webDir, o.workerDir, o.bodyDir} {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	// The files in it can be read by anybody (see keep), so the directory is what keeps them private.
	if err := os.Chmod(o.hostDir, 0o700); err != nil {
		return err
	}

	// ---- kept for good: the host directory ----
	randomHex := newServiceSecret
	// valkey_password is made whether or not Valkey is used (it is optional): it costs nothing, and
	// turning Valkey on later then needs no second run.
	for _, name := range []string{"db_admin_password", "db_password", "encryption_key", "body_db_password", "body_encryption_key", "valkey_password"} {
		if _, err := o.keep(o.hostDir, name, true, randomHex); err != nil {
			return err
		}
	}
	// The passwords are hex, so they are safe inside a URL. The connection strings are made from
	// the passwords on disk, so they stay right if a password was put there by hand.
	dsn := func(file, user, query string) func() (string, error) {
		return func() (string, error) {
			pw, err := existing(o.hostDir, file)
			if err != nil || pw == "" {
				return "", fmt.Errorf("read %s: %v", file, err)
			}
			return fmt.Sprintf("postgres://%s:%s@%s:5432/dexafix?%ssslmode=disable", user, pw, o.dbHost, query), nil
		}
	}
	if _, err := o.keep(o.hostDir, "dsn", true, dsn("db_password", "dexafix", "")); err != nil {
		return err
	}
	if _, err := o.keep(o.hostDir, "body_dsn", true, dsn("body_db_password", "dexafix_body", "search_path=body&")); err != nil {
		return err
	}

	if o.apiDir == "" {
		return nil
	}

	// ---- kept once: the secrets the processes share, with the api directory as the source ----
	service, err := o.keep(o.apiDir, fileServiceSecret, false, randomHex)
	if err != nil {
		return err
	}
	worker, err := o.keep(o.apiDir, fileWorkerSecret, false, randomHex)
	if err != nil {
		return err
	}
	bodySecret, err := o.keep(o.apiDir, fileBodySecret, false, randomHex)
	if err != nil {
		return err
	}
	tokenKey, err := o.keep(o.apiDir, fileBodyTokenKey, false, func() (string, error) {
		_, _, priv, err := bodyapi.GenerateKey()
		if err != nil {
			return "", err
		}
		return bodyapi.EncodePrivateKey(priv), nil
	})
	if err != nil {
		return err
	}
	// The URL of the optional Valkey server, with the password in it, for the api process only. It is
	// made from the password on disk, so it stays right if a password was put there by hand.
	if pw, err := existing(o.hostDir, "valkey_password"); err != nil {
		return err
	} else if pw != "" {
		if err := o.put(o.apiDir, fileValkeyURL, fmt.Sprintf("redis://:%s@%s:6379/0", pw, o.valkeyHost)); err != nil {
			return err
		}
	}
	if service == worker || service == bodySecret || worker == bodySecret {
		return errors.New("the service, worker and body secrets must all differ: they open different doors; delete the duplicates and run init again")
	}
	copies := []struct{ dir, name, value string }{
		{o.webDir, fileServiceSecret, service},
		{o.workerDir, fileWorkerSecret, worker},
		{o.bodyDir, fileBodySecret, bodySecret},
	}
	for _, c := range copies {
		if c.dir == "" {
			continue
		}
		if got, err := existing(c.dir, c.name); err != nil {
			return err
		} else if got == c.value {
			o.say("kept      %s", filepath.Join(c.dir, c.name))
			continue
		}
		if err := o.put(c.dir, c.name, c.value); err != nil {
			return err
		}
	}
	if o.bodyDir != "" {
		// The body service gets the public key only: it can check a token, never make one.
		priv, err := bodyapi.ParsePrivateKey(tokenKey)
		if err != nil {
			return err
		}
		if err := o.put(o.bodyDir, fileBodyTokenPub, bodyapi.EncodePublicKey(priv.Public().(ed25519.PublicKey))); err != nil {
			return err
		}
	}

	// ---- made again every time: the certificates of the two internal links ----
	web, err := mtls.GenerateValid(o.apiNames, "dexa-fix api", "dexa-fix web", o.validity)
	if err != nil {
		return err
	}
	type pemFile struct{ dir, name, content string }
	files := []pemFile{
		{o.apiDir, fileMTLSCA, web.CA}, {o.apiDir, fileMTLSCert, web.ServerCert}, {o.apiDir, fileMTLSKey, web.ServerKey},
	}
	// The web process and the workers share one client certificate: it proves the link, and the
	// service or worker secret proves the role.
	for _, d := range []string{o.webDir, o.workerDir} {
		if d != "" {
			files = append(files, pemFile{d, fileMTLSCA, web.CA}, pemFile{d, fileMTLSCert, web.ClientCert}, pemFile{d, fileMTLSKey, web.ClientKey})
		}
	}
	if o.bodyDir != "" {
		// The link to the body service has a CA of its own: a certificate of one link opens nothing
		// of the other.
		body, err := mtls.GenerateValid(o.bodyNames, "dexa-fix body", "dexa-fix api", o.validity)
		if err != nil {
			return err
		}
		files = append(files,
			pemFile{o.bodyDir, fileMTLSCA, body.CA}, pemFile{o.bodyDir, fileMTLSCert, body.ServerCert}, pemFile{o.bodyDir, fileMTLSKey, body.ServerKey},
			pemFile{o.apiDir, fileBodyMTLSCA, body.CA}, pemFile{o.apiDir, fileBodyMTLSCert, body.ClientCert}, pemFile{o.apiDir, fileBodyMTLSKey, body.ClientKey})
	}
	for _, f := range files {
		if err := o.put(f.dir, f.name, f.content); err != nil {
			return err
		}
	}
	return nil
}

// parseOwner reads "uid:gid" (or just "uid", which also sets the group); empty means "leave it".
func parseOwner(s string) (uid, gid int, err error) {
	if s == "" {
		return -1, -1, nil
	}
	u, g, _ := strings.Cut(s, ":")
	if g == "" {
		g = u
	}
	if uid, err = strconv.Atoi(u); err != nil || uid < 0 {
		return 0, 0, fmt.Errorf("--chown %q: use uid:gid, such as 65532:65532", s)
	}
	if gid, err = strconv.Atoi(g); err != nil || gid < 0 {
		return 0, 0, fmt.Errorf("--chown %q: use uid:gid, such as 65532:65532", s)
	}
	return uid, gid, nil
}

var secretsInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Create every secret and the mutual TLS certificates for the Compose deployment",
	Long: `Creates what deploy/docker-compose.yaml needs, in directories you name. Run it again at any time:
it never replaces a password, encryption key or shared secret that exists, and makes a new set of
mutual TLS certificates.

  --host-dir     passwords, connection strings and the two encryption keys. BACK THESE UP together with
                 the database: without an encryption key the encrypted values cannot be read again.
                 The files are created only when missing.
  --api-dir      the secrets of the api process, and the source of truth for the secrets it shares:
                 service_secret, worker_secret, body_secret, body_token_key (kept once), its
                 certificates (mtls_*, and body_mtls_* for the link to the body service), and
                 valkey_url (made from valkey_password in --host-dir, for the optional Valkey server)
  --web-dir      service_secret and the web process's client certificate
  --worker-dir   worker_secret and the same client certificate
  --body-dir     body_secret, body_token_pub (the public half: it can check tokens, not make them)
                 and the body service's certificate

The certificates are replaced atomically and are valid for --mtls-days days (397 by default, so
run init at least once a year; the Compose file runs it at every start). The processes read changed
certificate files again within 30 seconds, so they need no restart.`,
	Example: `# on the host, for a deployment that is not Compose:
dexa-fix secrets init --host-dir ./secrets/host --api-dir ./secrets/api --web-dir ./secrets/web \
  --worker-dir ./secrets/worker --body-dir ./secrets/body
# as the Compose init container: docker compose run --rm init`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		f := cmd.Flags()
		o := initOptions{log: cmd.ErrOrStderr()}
		o.hostDir, _ = f.GetString("host-dir")
		o.apiDir, _ = f.GetString("api-dir")
		o.webDir, _ = f.GetString("web-dir")
		o.workerDir, _ = f.GetString("worker-dir")
		o.bodyDir, _ = f.GetString("body-dir")
		o.dbHost, _ = f.GetString("db-host")
		o.valkeyHost, _ = f.GetString("valkey-host")
		o.apiNames, _ = f.GetStringSlice("api-name")
		o.bodyNames, _ = f.GetStringSlice("body-name")
		days, _ := f.GetInt("mtls-days")
		if days < 1 {
			return errors.New("--mtls-days must be at least 1")
		}
		o.validity = time.Duration(days) * 24 * time.Hour
		owner, _ := f.GetString("chown")
		var err error
		if o.uid, o.gid, err = parseOwner(owner); err != nil {
			return err
		}
		return o.run()
	},
}

func init() {
	f := secretsInitCmd.Flags()
	f.String("host-dir", "", "directory for the passwords, connection strings and encryption keys to back up (created only when missing)")
	f.String("api-dir", "", "directory of the api process (the source of the secrets it shares)")
	f.String("web-dir", "", "directory of the web process")
	f.String("worker-dir", "", "directory of the workers")
	f.String("body-dir", "", "directory of the body service")
	f.String("db-host", "db", "host name of the database in the connection strings")
	f.String("valkey-host", "valkey", "host name of the optional Valkey server in its URL")
	f.StringSlice("api-name", []string{"api"}, "host name the web process and workers reach the api process by (repeatable)")
	f.StringSlice("body-name", []string{"body"}, "host name the api process reaches the body service by (repeatable)")
	f.Int("mtls-days", 397, "days the mutual TLS certificates are valid")
	f.String("chown", "", "give the files in the process directories to uid:gid, such as 65532:65532 (needs permission to)")
	secretsCmd.AddCommand(secretsInitCmd)
}
