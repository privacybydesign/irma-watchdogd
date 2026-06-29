IRMA Watchdog
=============
`irma-watchdogd` keeps tabs on various part of the IRMA public infrastructure.
At the moment it checks:

 * Whether the online SchemeManager files are accessible and  properly signed.
 * Whether the publickeys of the issuers will expire soon.
 * Whether the TLS certificates of the webservers are (or soon will) expired
 * HTTP health checks being specified in the configuration

The tool has the following ways to report issues it finds:

 * Using an HTTP GET request (pull)
 * HTTP webhooks (push)
 * Slack integration

Installation
------------

Run

```
go install github.com/privacybydesign/irma-watchdogd
```

Create a `config.yaml` (see `config.yaml.example`) and simply run `irma-watchdogd`.

Diagnostics
-----------

If the watchdog reports hosts as unreachable that look fine from a browser, the
failure is likely specific to its vantage point (cluster DNS, network path, or
the pod's CPU budget). `scripts/probe.sh` reproduces the same checks on a tight
loop and logs per-phase timing (DNS / connect / TLS / first byte) so you can see
which phase degrades during a blip. Run it from a debug pod in the watchdog's
namespace and, at the same time, from a workstation, then compare. See the
header of the script for the exact commands.
