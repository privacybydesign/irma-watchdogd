IRMA Watchdog
=============
`irma-watchdogd` keeps tabs on various part of the IRMA public infrastructure.
At the moment it checks.

 * Whether the online SchemeManager files are accessable and  properly signed.
 * Whether the publickeys of the issuers will expire soon.

Installation
------------

Run

```
go get github.com/privacybydesign/irma-wachtdogd
```

Create a `config.yaml` (see `config.yaml.example`) and simply run `irma-watchdogd`.
