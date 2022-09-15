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
