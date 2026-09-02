# ResMan operator helper scripts

The scripts in this directory are optional operator utilities. ResMan does not invoke
them automatically, and installing the package does not configure an SMTP server or
mail credentials.

## `sendmail.sh`

`sendmail.sh` builds a MIME message and sends it directly to an SMTP server with
`curl`. RPM and Debian packages install it at:

```text
/usr/share/doc/resman/scripts/sendmail.sh
```

### Requirements

- Bash
- `curl` built with SMTP support
- the standard `base64`, `basename`, `cat`, and `date` utilities
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
/usr/share/doc/resman/scripts/sendmail.sh \
  -f sender@example.com \
  -t operator@example.com \
  -s 'ResMan notification' \
  -S smtp.example.com \
  -P 587 \
  -u smtp-user \
  -w 'smtp-password' \
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
| `-u user` | SMTP authentication username. Authentication is enabled when this is set. |
| `-w password` | SMTP authentication password. |
| `-A type` | Authentication mechanism: `plain` or `login`; defaults to `plain`. |
| `-T` | Require STARTTLS. |
| `-a file` | Attach a file; repeat for multiple attachments. |
| `-b file` | Read the text body from a file instead of standard input. |
| `-h` | Print the built-in help and exit. |

The command exits with status zero only when message generation and SMTP delivery
succeed. Missing required addresses, an invalid authentication type, missing
attachments, unreadable input, utility failures, TLS failures, and SMTP failures
produce a non-zero status.

### Security notes

- Treat sender, recipient, subject, body, and attachment names as trusted operator
  input. The helper does not sanitize untrusted text for use in mail headers.
- The value passed with `-w` is forwarded to `curl` on its command line. It can appear
  in shell history and may be visible to privileged local processes. Use a dedicated,
  least-privilege SMTP credential and avoid shared interactive shells.
- Prefer `-T` whenever the SMTP server supports STARTTLS. Without it, message contents
  and credentials can cross the network without transport encryption.
- Protect body and attachment files according to their contents. The helper does not
  change their permissions.
