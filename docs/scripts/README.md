# ResMan operator helper scripts

The scripts in this directory are optional operator utilities. Installing the package
does not configure an SMTP server, a recipient, or mail credentials.

## `resman-sendmail-hook.sh`

`resman-sendmail-hook.sh` is the example adapter that implements ResMan's no-argument
limit-hook contract. It consumes the documented `RESMAN_LIMIT_*` environment,
constructs a text notification, and delegates SMTP delivery to `sendmail.sh`.
`sendmail.sh` remains a generic command-line helper and is not itself a ResMan hook.

Do not edit or configure the copy under `/usr/share/doc`: a package upgrade can replace
it. Copy it to a trusted path, edit the static sender and recipient, and protect it
according to the limit-hook path rules:

```bash
sudo install -d -o root -g root -m 0755 /usr/local/libexec/resman
sudo install -o root -g root -m 0755 \
  /usr/share/doc/resman/scripts/resman-sendmail-hook.sh \
  /usr/local/libexec/resman/resman-sendmail-hook.sh
sudo editor /usr/local/libexec/resman/resman-sendmail-hook.sh
```

The example uses an unauthenticated SMTP relay. It contains no password option and
does not place SMTP credentials on a process command line. Sites requiring
authentication should treat the transport customization as operator-owned code and
use a protected credential mechanism suitable for their mail system; do not add a
password argument to the example adapter.

Configure ResMan with the copied adapter and the dedicated non-root identity:

```ini
LIMIT_HOOK_ENABLED=true
LIMIT_HOOK_SCRIPT=/usr/local/libexec/resman/resman-sendmail-hook.sh
LIMIT_HOOK_SCRIPT_USER=resman-hook
LIMIT_HOOK_SCRIPT_GROUP=resman-hook
```

ResMan passes no positional arguments. Event values such as the UID, username,
timestamp, applied CPU Points class, and RAM coverage come from the fixed
`RESMAN_LIMIT_*` environment. The adapter rejects unexpected arguments, missing or
malformed required event fields, control characters in event data, unconfigured
sender or recipient placeholders, an invalid SMTP port, and an unavailable helper.
Delivery failures propagate to ResMan as a failed hook outcome without weakening the
limit already applied.

## `sendmail.sh`

`sendmail.sh` is a generic, freely provided example utility. It builds a MIME message
and sends it directly to an SMTP server with `curl`. It requires command-line
arguments and therefore must not be configured directly as `LIMIT_HOOK_SCRIPT`. RPM
and Debian packages install both examples at:

```text
/usr/share/doc/resman/scripts/sendmail.sh
/usr/share/doc/resman/scripts/resman-sendmail-hook.sh
```

### Requirements

- Bash
- `curl` built with SMTP support
- the standard `base64`, `basename`, `cat`, `date`, and `stat` utilities
- network access to the selected SMTP server

The helper does not install these programs or validate the SMTP server configuration.

### Basic use

Send a text body from standard input:

```bash
printf '%s\n' 'ResMan notification' |
  /usr/share/doc/resman/scripts/sendmail.sh \
    -f sender@example.com \
    -t operator@example.com \
    -s 'ResMan notification'
```

Read the body from a file and attach a report:

```bash
/usr/share/doc/resman/scripts/sendmail.sh \
  -f sender@example.com \
  -t operator@example.com \
  -c oncall@example.com \
  -s 'ResMan report' \
  -b /path/to/body.txt \
  -a /path/to/report.txt
```

Use authenticated SMTP with mandatory STARTTLS:

```bash
sudo install -o root -g root -m 0600 /dev/null /etc/resman/sendmail.netrc
sudo editor /etc/resman/sendmail.netrc
```

The protected file uses curl's netrc syntax:

```text
machine smtp.example.com
login smtp-user
password replace-with-the-smtp-password
```

Then pass only its path to the helper:

```bash
/usr/share/doc/resman/scripts/sendmail.sh \
  -f sender@example.com \
  -t operator@example.com \
  -s 'ResMan notification' \
  -S smtp.example.com \
  -P 587 \
  -N /etc/resman/sendmail.netrc \
  -A plain \
  -T \
  -b /path/to/body.txt
```

`-T` requests STARTTLS and requires TLS before the message is sent. Without `-T`, the
helper uses plain SMTP. Certificate verification follows the local `curl` defaults.

### Options

| Option | Meaning |
|---|---|
| `-f address` | Envelope and header sender; required. |
| `-t address` | Primary envelope and header recipient; required. |
| `-c address` | Carbon-copy recipient; repeat for multiple recipients. |
| `-s subject` | Message subject. |
| `-S server` | SMTP server; defaults to `localhost`. |
| `-P port` | SMTP port; defaults to `25`. |
| `-N file` | Authenticate with a protected netrc file. The path, never its contents, is passed to `curl`. |
| `-A type` | Authentication mechanism used with `-N`: `plain` or `login`; defaults to `plain`. |
| `-T` | Require STARTTLS. |
| `-a file` | Attach a file; repeat for multiple attachments. |
| `-b file` | Read the text body from a file instead of standard input. |
| `-h` | Print the built-in help and exit. |

The command exits with status zero only when message generation and SMTP delivery
succeed. Missing required addresses, an invalid authentication type, missing
attachments, unreadable input, utility failures, TLS failures, and SMTP failures
produce a non-zero status.

Every invocation is bounded by a 10-second connection timeout and a 60-second total
transfer timeout. Set `SENDMAIL_CONNECT_TIMEOUT_SECONDS` and
`SENDMAIL_MAX_TIME_SECONDS` to positive integer seconds to override them; the
connection timeout must not exceed the total timeout. A timeout is a delivery
failure and produces a non-zero status.

### Security notes

- Treat sender, recipient, subject, body, and attachment names as trusted operator
  input. The helper does not sanitize untrusted text for use in mail headers.
- The helper deliberately has no username or password option. It rejects the former
  `-u` and `-w` interface so credentials cannot enter shell history or a process
  argument list. `-N` accepts only a readable regular file owned by the effective
  user, rejects symbolic links, and rejects any group or other permission bits. Use a
  dedicated, least-privilege SMTP credential and keep the file at mode `0600` or
  stricter.
- Prefer `-T` whenever the SMTP server supports STARTTLS. Without it, message contents
  and credentials can cross the network without transport encryption.
- Protect body and attachment files according to their contents. The helper does not
  change their permissions.
