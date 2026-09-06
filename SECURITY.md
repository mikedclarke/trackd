# Security

trackd is meant to run on a private network. It has no TLS of its own and its only authentication is a bearer token per agent, so put it behind Tailscale, a VPN or a reverse proxy that terminates TLS, and never expose port 8484 to the internet directly.

Treat database snapshots and JSONL dumps as sensitive: they contain the full issue history and the hashed token table.

## Reporting a vulnerability

Please do not open a public issue for a security problem. Use GitHub's private vulnerability reporting on this repository (Security tab, "Report a vulnerability"). You will get an acknowledgement within a few days and a fix or a clear answer as soon as one exists. There is no bug bounty.
