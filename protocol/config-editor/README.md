# ResMan configuration-editor protocol

The JSON Schema and compatibility fixtures in this directory describe the
public `resman.config-editor.v1` MCP wire contract. They contain no daemon
implementation logic and may be consumed by an independent client without
importing the ResMan Go module.

`schema-v1.json` defines the editor snapshot plus update request and terminal-result
definitions. `snapshot-v1.json`, `update-request-v1.json`, and
`update-result-v1.json` are compatibility fixtures. Sensitive values are deliberately
absent: editor clients send only explicitly changed secrets and must never expect a
secret value or complete source file in a response.

Source device, inode, and size identifiers are decimal strings on the wire. This
preserves their full integer identity when a browser client parses and returns a
revision; clients must not convert them through an IEEE-754 number.

These files are licensed under the Apache License 2.0 as stated in `LICENSE`.
The ResMan daemon and the remainder of this repository remain licensed under
GPL-3.0-or-later.
